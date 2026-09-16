package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/glennprays/catet-backend/internal/testkit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goodPassword clears both length and blocklist checks; wrongPassword
// is a different password that also would.
const (
	goodPassword  = "kata-sandi-rahasia"
	otherPassword = "kata-sandi-lainnya"
)

// memoryCodeStore is the OTP table, per phone, in memory: the tests
// need to read the code that was "sent", which a real random code in
// Postgres would not give them.
type memoryCodeStore struct {
	mu     sync.Mutex
	latest map[string]*entity.OtpCode
}

func newMemoryCodeStore() *memoryCodeStore {
	return &memoryCodeStore{latest: map[string]*entity.OtpCode{}}
}

func (s *memoryCodeStore) Create(_ context.Context, phone, codeHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest[phone] = &entity.OtpCode{
		ID:        uuid.New(),
		Phone:     phone,
		CodeHash:  codeHash,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
	}
	return nil
}

func (s *memoryCodeStore) Latest(_ context.Context, phone string) (*entity.OtpCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[phone], nil
}

func (s *memoryCodeStore) IncrementAttempts(_ context.Context, id uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	code := s.byID(id)
	code.Attempts++
	return code.Attempts, nil
}

func (s *memoryCodeStore) Consume(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID(id).Consumed = true
	return nil
}

func (s *memoryCodeStore) byID(id uuid.UUID) *entity.OtpCode {
	for _, code := range s.latest {
		if code.ID == id {
			return code
		}
	}
	return nil
}

// fakeGateway is a gateway.Sender that records instead of sending, and
// answers the lid lookup the way a configured gateway would.
type fakeGateway struct {
	mu   sync.Mutex
	sent []string
	lid  string
}

func (g *fakeGateway) SendText(_ context.Context, _, text, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, text)
	return uuid.NewString(), nil
}

func (g *fakeGateway) React(context.Context, string, string, string, string) error { return nil }

func (g *fakeGateway) SetTyping(context.Context, string, bool) error { return nil }

func (g *fakeGateway) ContactLID(context.Context, string) (string, error) { return g.lid, nil }

type authFixture struct {
	svc     *AuthService
	tokens  *TokenService
	users   *repository.UserRepository
	invites *repository.InviteRepository
	resets  *repository.PasswordResetRepository
	cats    *repository.CategoryRepository
	sources *repository.SourceRepository
	codes   *memoryCodeStore
	otpSent *fakeOtpSender
	gw      *fakeGateway
	pool    *pgxpool.Pool
	ctx     context.Context
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	pool := testkit.NewDB(t)

	f := &authFixture{
		users:   repository.NewUserRepository(pool),
		invites: repository.NewInviteRepository(pool),
		resets:  repository.NewPasswordResetRepository(pool),
		cats:    repository.NewCategoryRepository(pool),
		sources: repository.NewSourceRepository(pool),
		codes:   newMemoryCodeStore(),
		otpSent: &fakeOtpSender{},
		gw:      &fakeGateway{lid: "55"},
		tokens:  NewTokenService("test-secret"),
		pool:    pool,
		ctx:     context.Background(),
	}
	f.svc = NewAuthService(
		f.users, f.invites, f.resets,
		NewOtpService(f.codes, f.otpSent, 5*time.Minute),
		f.tokens,
		service.NewOnboardingService(f.cats, f.sources),
		f.gw,
	)
	return f
}

// createUser registers an account with a password, the way Register
// would leave it.
func (f *authFixture) createUser(t *testing.T, phone, name, password string) *entity.User {
	t.Helper()
	hash, err := HashPassword(password)
	require.NoError(t, err)
	u, err := f.users.Create(t.Context(), &entity.User{
		Phone: phone, DisplayName: name, PasswordHash: &hash,
	})
	require.NoError(t, err)
	return u
}

// invite issues an open invite directly, returning the plaintext token
// the register link would carry.
func (f *authFixture) invite(t *testing.T, phone string, createdBy uuid.UUID) string {
	t.Helper()
	token, err := randomToken()
	require.NoError(t, err)
	_, err = f.invites.Create(t.Context(), phone, HashToken(token), createdBy, time.Now().Add(time.Hour))
	require.NoError(t, err)
	return token
}

// lastCode reads the code the OTP sender just delivered.
func (f *authFixture) lastCode(t *testing.T) string {
	t.Helper()
	require.NotEmpty(t, f.otpSent.codes, "no code was sent")
	return f.otpSent.codes[len(f.otpSent.codes)-1]
}

func appError(t *testing.T, err error) *apperror.Error {
	t.Helper()
	var ae *apperror.Error
	require.True(t, errors.As(err, &ae), "want *apperror.Error, got %v", err)
	return ae
}

// requireAccountState asserts the 403 the dashboard branches on: the
// field/rule pair is what tells the login form to say "locked" rather
// than "wrong password".
func requireAccountState(t *testing.T, err error, rule string) {
	t.Helper()
	ae := appError(t, err)
	assert.Equal(t, apperror.KindForbidden, ae.Kind)
	require.Len(t, ae.Details, 1)
	assert.Equal(t, "account", ae.Details[0].Field)
	assert.Equal(t, rule, ae.Details[0].Rule)
}

// Five wrong passwords lock the account for fifteen minutes. The lock
// has to beat a correct password — otherwise an attacker who finally
// guesses right walks in — and it has to expire, or one bad afternoon
// costs the user their account.
func TestLogin_FiveFailuresLockEvenAgainstTheRightPassword(t *testing.T) {
	f := newAuthFixture(t)
	u := f.createUser(t, "+628123456789", "Owner", goodPassword)

	for i := 1; i <= 4; i++ {
		_, _, err := f.svc.Login(f.ctx, u.Phone, "salah-sekali-2026", "10.0.0.1")
		assert.Equal(t, apperror.KindUnauthorized, appError(t, err).Kind, "attempt %d", i)
	}

	// The fifth failure is reported as the lock it caused, not as another
	// wrong password: the user must learn why the next try fails.
	_, _, err := f.svc.Login(f.ctx, u.Phone, "salah-sekali-2026", "10.0.0.1")
	requireAccountState(t, err, "locked")

	_, _, err = f.svc.Login(f.ctx, u.Phone, goodPassword, "10.0.0.1")
	requireAccountState(t, err, "locked")

	// Once the window passes the right password works again.
	f.svc.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	token, _, err := f.svc.Login(f.ctx, u.Phone, goodPassword, "10.0.0.1")
	require.NoError(t, err)
	assert.NotEmpty(t, token)
}

func TestLogin_DeactivatedAccountIsToldSo(t *testing.T) {
	f := newAuthFixture(t)
	u := f.createUser(t, "+628123456789", "Owner", goodPassword)
	now := time.Now().UTC()
	require.NoError(t, f.users.SetDeactivated(f.ctx, u.ID, &now))

	_, _, err := f.svc.Login(f.ctx, u.Phone, goodPassword, "10.0.0.1")
	requireAccountState(t, err, "deactivated")
}

// An unknown number and a wrong password must be indistinguishable:
// login is public, so any difference turns it into a directory of who
// has an account.
func TestLogin_UnknownNumberAndWrongPasswordAnswerIdentically(t *testing.T) {
	f := newAuthFixture(t)
	u := f.createUser(t, "+628123456789", "Owner", goodPassword)

	_, _, unknownErr := f.svc.Login(f.ctx, "+628999999999", goodPassword, "10.0.0.1")
	_, _, wrongErr := f.svc.Login(f.ctx, u.Phone, "salah-sekali-2026", "10.0.0.1")

	unknown, wrong := appError(t, unknownErr), appError(t, wrongErr)
	assert.Equal(t, apperror.KindUnauthorized, unknown.Kind)
	assert.Equal(t, apperror.KindUnauthorized, wrong.Kind)
	assert.Equal(t, unknown.Message, wrong.Message)
	assert.Empty(t, unknown.Details, "no detail may hint that the number is unknown")
	assert.Empty(t, wrong.Details)
}

// A code only goes to a number that is allowed one. The endpoint always
// answers "sent", so the silence has to happen here, not in the reply.
func TestRequestCode_OnlyRegisteredOrInvitedNumbersGetOne(t *testing.T) {
	f := newAuthFixture(t)
	admin := f.createUser(t, "+628123456789", "Owner", goodPassword)

	require.NoError(t, f.svc.RequestCode(f.ctx, "+628999999999", "10.0.0.1"))
	assert.Empty(t, f.otpSent.codes, "an unknown, uninvited number must receive nothing")

	f.invite(t, "+628999999999", admin.ID)
	require.NoError(t, f.svc.RequestCode(f.ctx, "+628999999999", "10.0.0.1"))
	assert.Len(t, f.otpSent.codes, 1, "an invited number needs a code to sign up")

	require.NoError(t, f.svc.RequestCode(f.ctx, admin.Phone, "10.0.0.1"))
	assert.Len(t, f.otpSent.codes, 2, "a registered number needs a code to reset its password")
}

// Signup is the only way an account comes into existence, so it has to
// leave a usable one: a seeded ledger, a closed invite, and a session.
func TestRegister_SeedsTheLedgerAndSpendsTheInvite(t *testing.T) {
	f := newAuthFixture(t)
	admin := f.createUser(t, "+628123456789", "Owner", goodPassword)
	const invitee = "+628999999999"
	token := f.invite(t, invitee, admin.ID)

	require.NoError(t, f.svc.RequestCode(f.ctx, invitee, "10.0.0.2"))
	session, expiresAt, err := f.svc.Register(f.ctx, token, f.lastCode(t), " Budi ", goodPassword)
	require.NoError(t, err)
	assert.True(t, expiresAt.After(time.Now()))

	created, err := f.users.GetByPhone(f.ctx, invitee)
	require.NoError(t, err)
	require.NotNil(t, created)
	assert.Equal(t, "Budi", created.DisplayName, "the name is stored trimmed")
	require.NotNil(t, created.WhatsAppLID)
	assert.Equal(t, "55", *created.WhatsAppLID, "the lid is what lets the bot recognise them")

	sess, err := f.tokens.Verify(session)
	require.NoError(t, err)
	assert.Equal(t, created.ID, sess.UserID)
	assert.Equal(t, created.SessionEpoch, sess.Epoch)

	// An empty Categories page and an empty Sources rail would make the
	// first capture impossible.
	scoped := scope.WithUserID(f.ctx, created.ID)
	cats, err := f.cats.List(scoped, false)
	require.NoError(t, err)
	assert.Len(t, cats, 8, "6 roots plus the two children under Tagihan")
	sources, err := f.sources.List(scoped, false)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, service.DefaultSourceName, sources[0].Name)

	open, err := f.invites.ListOpen(f.ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "the invite is spent the moment the account exists")

	// The same link must not create a second account.
	_, _, err = f.svc.Register(f.ctx, token, "123456", "Budi Lagi", goodPassword)
	assert.Equal(t, apperror.KindNotFound, appError(t, err).Kind)
}

func TestRegister_WeakPasswordIsRefusedBeforeTheCodeIsSpent(t *testing.T) {
	f := newAuthFixture(t)
	admin := f.createUser(t, "+628123456789", "Owner", goodPassword)
	const invitee = "+628999999999"
	token := f.invite(t, invitee, admin.ID)
	require.NoError(t, f.svc.RequestCode(f.ctx, invitee, "10.0.0.2"))
	code := f.lastCode(t)

	_, _, err := f.svc.Register(f.ctx, token, code, "Budi", "short")
	assert.Equal(t, apperror.KindValidation, appError(t, err).Kind)

	// The code survived, so the invitee only has to fix the password.
	_, _, err = f.svc.Register(f.ctx, token, code, "Budi", goodPassword)
	require.NoError(t, err)
}

// A reset token is single-use and short-lived: it is a bearer secret
// that replaces a password, so a replay or a stale link must not work.
func TestReset_TokenIsSingleUseAndExpires(t *testing.T) {
	f := newAuthFixture(t)
	u := f.createUser(t, "+628123456789", "Owner", goodPassword)

	require.NoError(t, f.svc.RequestCode(f.ctx, u.Phone, "10.0.0.1"))
	resetToken, _, err := f.svc.VerifyResetCode(f.ctx, u.Phone, f.lastCode(t), "10.0.0.1")
	require.NoError(t, err)

	session, _, err := f.svc.CompleteReset(f.ctx, resetToken, otherPassword)
	require.NoError(t, err)
	assert.NotEmpty(t, session, "the user proved the number, so the reset ends logged in")

	_, _, err = f.svc.CompleteReset(f.ctx, resetToken, goodPassword)
	assert.Equal(t, apperror.KindBadRequest, appError(t, err).Kind)

	// An expired row is just as dead as a consumed one.
	expired, err := randomToken()
	require.NoError(t, err)
	require.NoError(t, f.resets.Create(f.ctx, u.ID, HashToken(expired), time.Now().Add(-time.Minute)))
	_, _, err = f.svc.CompleteReset(f.ctx, expired, goodPassword)
	assert.Equal(t, apperror.KindBadRequest, appError(t, err).Kind)

	// The new password is the one that works now.
	_, _, err = f.svc.Login(f.ctx, u.Phone, otherPassword, "10.0.0.1")
	require.NoError(t, err)
}

// Changing a password needs the current one (a borrowed session must not
// be enough to take the account over) and signs every device out.
func TestChangePassword_RequiresCurrentAndRevokesSessions(t *testing.T) {
	f := newAuthFixture(t)
	u := f.createUser(t, "+628123456789", "Owner", goodPassword)

	token, _, err := f.svc.Login(f.ctx, u.Phone, goodPassword, "10.0.0.1")
	require.NoError(t, err)
	issued, err := f.tokens.Verify(token)
	require.NoError(t, err)

	err = f.svc.ChangePassword(f.ctx, u.ID, "salah-sekali-2026", otherPassword)
	assert.Equal(t, apperror.KindBadRequest, appError(t, err).Kind)

	require.NoError(t, f.svc.ChangePassword(f.ctx, u.ID, goodPassword, otherPassword))

	after, err := f.users.GetByID(f.ctx, u.ID)
	require.NoError(t, err)
	assert.NotEqual(t, issued.Epoch, after.SessionEpoch,
		"the token issued before the change must no longer match the stored epoch")

	_, _, err = f.svc.Login(f.ctx, u.Phone, goodPassword, "10.0.0.1")
	assert.Equal(t, apperror.KindUnauthorized, appError(t, err).Kind, "the old password is gone")
}

// "Log out everywhere" is the same mechanism with no password: the
// epoch moves, so every issued token stops matching.
func TestLogoutAll_MovesTheEpoch(t *testing.T) {
	f := newAuthFixture(t)
	u := f.createUser(t, "+628123456789", "Owner", goodPassword)

	require.NoError(t, f.svc.LogoutAll(f.ctx, u.ID))

	after, err := f.users.GetByID(f.ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, u.SessionEpoch+1, after.SessionEpoch)
}
