package auth

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/catet-backend/internal/scope"
	"github.com/glennprays/catet-backend/internal/service"
	"github.com/google/uuid"
)

// resetTTL is how long a password-reset token lives. Short: the user is
// standing in front of the form when it is issued.
const resetTTL = 15 * time.Minute

// maxDisplayNameRunes bounds the name shown in Settings and the users
// list.
const maxDisplayNameRunes = 60

// throttleWindow is the sliding window every auth limit shares.
const throttleWindow = 15 * time.Minute

// dummyHash makes a login against an unknown number cost the same
// argon2 work as one against a real account, so response time does not
// enumerate the user table. It hashes a random secret generated at
// startup: nobody can ever match it.
var dummyHash = func() string {
	secret, err := randomToken()
	if err != nil {
		panic("auth: generate dummy password: " + err.Error())
	}
	hash, err := HashPassword(secret)
	if err != nil {
		panic("auth: hash dummy password: " + err.Error())
	}
	return hash
}()

// AuthService owns everything a session is made of: login, the two OTP
// errands (verify a phone at signup, reset a password), and the
// self-service account operations.
type AuthService struct {
	users      *repository.UserRepository
	invites    *repository.InviteRepository
	resets     *repository.PasswordResetRepository
	otp        *OtpService
	tokens     *TokenService
	onboarding *service.OnboardingService
	gw         gateway.Sender

	// Sliding windows. Login is limited per phone (one account under
	// attack) and per IP (one attacker across accounts); the OTP and
	// reset endpoints are limited per IP only, because their per-phone
	// brake is the existing one-minute code cooldown.
	loginPhone *Throttle
	loginIP    *Throttle
	otpIP      *Throttle
	resetIP    *Throttle

	now func() time.Time
}

func NewAuthService(
	users *repository.UserRepository,
	invites *repository.InviteRepository,
	resets *repository.PasswordResetRepository,
	otp *OtpService,
	tokens *TokenService,
	onboarding *service.OnboardingService,
	gw gateway.Sender,
) *AuthService {
	return &AuthService{
		users:      users,
		invites:    invites,
		resets:     resets,
		otp:        otp,
		tokens:     tokens,
		onboarding: onboarding,
		gw:         gw,
		loginPhone: NewThrottle(10, throttleWindow),
		loginIP:    NewThrottle(30, throttleWindow),
		otpIP:      NewThrottle(10, throttleWindow),
		resetIP:    NewThrottle(10, throttleWindow),
		now:        time.Now,
	}
}

// errTooMany is the one answer every throttle gives: naming the limit
// would tell an attacker which one they hit.
func errTooMany() error {
	return apperror.TooManyRequests("terlalu banyak percobaan; coba lagi nanti")
}

// errBadCredentials is deliberately the same for an unknown number and
// a wrong password.
func errBadCredentials() error {
	return apperror.Unauthorized("nomor atau kata sandi salah")
}

func errDeactivated() error {
	return apperror.Forbidden("akun dinonaktifkan").WithDetails(apperror.FieldError{
		Field:   "account",
		Rule:    "deactivated",
		Message: "akun dinonaktifkan",
	})
}

func errLocked() error {
	return apperror.Forbidden("akun terkunci sementara, coba lagi dalam 15 menit").WithDetails(apperror.FieldError{
		Field:   "account",
		Rule:    "locked",
		Message: "akun terkunci sementara, coba lagi dalam 15 menit",
	})
}

// Login exchanges phone + password for a session token.
//
// The order of the checks is the policy: throttles first (they cost
// nothing), then the account's own state, then the password. A
// deactivated or locked account is told so — hiding it would leave the
// user retrying a password that is in fact correct — but an unknown
// number and a wrong password are indistinguishable.
func (s *AuthService) Login(ctx context.Context, phone, password, ip string) (string, time.Time, error) {
	if !s.loginIP.Allow(ip) {
		return "", time.Time{}, errTooMany()
	}

	normalized, err := NormalizePhone(phone)
	if err != nil {
		// An unparseable number is not a hint worth giving: answer as if
		// the credentials were simply wrong.
		return "", time.Time{}, errBadCredentials()
	}
	if !s.loginPhone.Allow(normalized) {
		return "", time.Time{}, errTooMany()
	}

	u, err := s.users.GetByPhone(ctx, normalized)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to load user").Wrap(err)
	}
	if u == nil || u.PasswordHash == nil {
		// Spend the same time an existing account would.
		_, _ = VerifyPassword(dummyHash, password)
		return "", time.Time{}, errBadCredentials()
	}
	if u.DeactivatedAt != nil {
		return "", time.Time{}, errDeactivated()
	}
	if u.LockedUntil != nil && s.now().Before(*u.LockedUntil) {
		return "", time.Time{}, errLocked()
	}

	ok, err := VerifyPassword(*u.PasswordHash, password)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to verify password").Wrap(err)
	}
	if !ok {
		_, lockedUntil, failErr := s.users.RecordLoginFailure(ctx, u.ID)
		if failErr != nil {
			return "", time.Time{}, apperror.Internal("failed to record login failure").Wrap(failErr)
		}
		// Report the lock on the attempt that caused it, not on the next
		// one: otherwise the user is told "wrong password" and then
		// locked out with no explanation.
		if lockedUntil != nil && s.now().Before(*lockedUntil) {
			return "", time.Time{}, errLocked()
		}
		return "", time.Time{}, errBadCredentials()
	}

	if err := s.users.RecordLoginSuccess(ctx, u.ID); err != nil {
		return "", time.Time{}, apperror.Internal("failed to record login").Wrap(err)
	}
	return s.issue(u)
}

// issue mints the session for an account at its current epoch.
func (s *AuthService) issue(u *entity.User) (string, time.Time, error) {
	token, expiresAt, err := s.tokens.Issue(u.ID, u.SessionEpoch)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to issue session").Wrap(err)
	}
	return token, expiresAt, nil
}

// RequestCode sends a WhatsApp code to a number that is allowed to have
// one: a registered account (password reset) or a number holding an open
// invite (signup). Every other number gets nil — the handler answers
// `{sent: true}` either way, so the endpoint cannot be used to discover
// who has an account.
func (s *AuthService) RequestCode(ctx context.Context, phone, ip string) error {
	if !s.otpIP.Allow(ip) {
		return errTooMany()
	}
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return nil
	}

	u, err := s.users.GetByPhone(ctx, normalized)
	if err != nil {
		return apperror.Internal("failed to load user").Wrap(err)
	}
	eligible := u != nil && u.DeactivatedAt == nil
	if !eligible {
		inv, err := s.invites.OpenByPhoneDigits(ctx, gateway.DigitsOnly(normalized))
		if err != nil {
			return apperror.Internal("failed to load invite").Wrap(err)
		}
		eligible = inv != nil
	}
	if !eligible {
		return nil
	}
	// A cooldown 429 from here is not enumerable: it only happens after
	// a code was already sent to this number, which the caller knows.
	return s.otp.Request(ctx, normalized)
}

// InviteInfo resolves a register link so the page can show which number
// it is for.
func (s *AuthService) InviteInfo(ctx context.Context, token string) (*entity.Invite, error) {
	inv, err := s.invites.GetOpenByTokenHash(ctx, HashToken(token))
	if err != nil {
		return nil, apperror.Internal("failed to load invite").Wrap(err)
	}
	if inv == nil {
		return nil, apperror.NotFound("undangan tidak ditemukan atau sudah kedaluwarsa")
	}
	return inv, nil
}

// Register turns an invite plus a WhatsApp code into an account and logs
// it straight in.
//
// The order matters on a crash: the user row is created before the
// ledger is seeded and the invite is closed last, so a failure halfway
// leaves a usable account rather than a burnt invite.
func (s *AuthService) Register(
	ctx context.Context, token, code, displayName, password string,
) (string, time.Time, error) {
	inv, err := s.InviteInfo(ctx, token)
	if err != nil {
		return "", time.Time{}, err
	}
	name := strings.TrimSpace(displayName)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxDisplayNameRunes {
		return "", time.Time{}, apperror.Validation("display name validation failed").WithDetails(apperror.FieldError{
			Field:   "display_name",
			Rule:    "len",
			Message: "Nama wajib diisi, maksimal 60 karakter",
		})
	}
	if err := ValidatePassword(password); err != nil {
		return "", time.Time{}, err
	}
	// The code is checked against the invited number, not against
	// anything the client sent: the invite is what binds the two.
	if err := s.otp.Check(ctx, inv.Phone, code); err != nil {
		return "", time.Time{}, err
	}

	existing, err := s.users.GetByPhone(ctx, inv.Phone)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to load user").Wrap(err)
	}
	if existing != nil {
		return "", time.Time{}, apperror.Conflict("nomor sudah terdaftar")
	}

	hash, err := HashPassword(password)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to hash password").Wrap(err)
	}
	u, err := s.users.Create(ctx, &entity.User{
		Phone:        inv.Phone,
		DisplayName:  name,
		PasswordHash: &hash,
	})
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to create user").Wrap(err)
	}

	// Best-effort: WhatsApp may address this person by an opaque lid, and
	// knowing it now means the bot recognises their first message. A
	// gateway that cannot tell us is not a failed signup — the harness
	// learns it later.
	if lid, err := s.gw.ContactLID(ctx, u.Phone); err == nil && lid != "" {
		if err := s.users.SetLID(ctx, u.ID, lid); err != nil {
			return "", time.Time{}, apperror.Internal("failed to store whatsapp lid").Wrap(err)
		}
	}

	if err := s.onboarding.SeedDefaults(scope.WithUserID(ctx, u.ID)); err != nil {
		return "", time.Time{}, apperror.Internal("failed to seed account").Wrap(err)
	}
	if err := s.invites.MarkUsed(ctx, inv.ID); err != nil {
		return "", time.Time{}, apperror.Internal("failed to close invite").Wrap(err)
	}
	return s.issue(u)
}

// VerifyResetCode trades a WhatsApp code for a short-lived reset token.
// An unknown number answers exactly like a wrong code: the reset form
// must not reveal who has an account.
func (s *AuthService) VerifyResetCode(ctx context.Context, phone, code, ip string) (string, time.Time, error) {
	if !s.resetIP.Allow(ip) {
		return "", time.Time{}, errTooMany()
	}
	badCode := apperror.BadRequest("code is invalid or expired")

	normalized, err := NormalizePhone(phone)
	if err != nil {
		return "", time.Time{}, badCode
	}
	u, err := s.users.GetByPhone(ctx, normalized)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to load user").Wrap(err)
	}
	if u == nil || u.DeactivatedAt != nil {
		return "", time.Time{}, badCode
	}
	if err := s.otp.Check(ctx, normalized, code); err != nil {
		return "", time.Time{}, err
	}

	token, err := randomToken()
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to generate reset token").Wrap(err)
	}
	expiresAt := s.now().Add(resetTTL)
	if err := s.resets.Create(ctx, u.ID, HashToken(token), expiresAt); err != nil {
		return "", time.Time{}, apperror.Internal("failed to store reset token").Wrap(err)
	}
	return token, expiresAt, nil
}

// CompleteReset sets the new password and returns a session: the user
// proved the number moments ago, so making them type the new password
// again on a login screen would add nothing.
func (s *AuthService) CompleteReset(ctx context.Context, resetToken, password string) (string, time.Time, error) {
	if err := ValidatePassword(password); err != nil {
		return "", time.Time{}, err
	}
	userID, err := s.resets.Consume(ctx, HashToken(resetToken))
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to consume reset token").Wrap(err)
	}
	if userID == nil {
		return "", time.Time{}, apperror.BadRequest("link reset tidak valid atau sudah kedaluwarsa")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to hash password").Wrap(err)
	}
	if err := s.users.SetPassword(ctx, *userID, hash); err != nil {
		return "", time.Time{}, apperror.Internal("failed to store password").Wrap(err)
	}
	// SetPassword bumped the epoch, so the session has to be minted from
	// the reloaded row — a token at the old epoch would already be dead.
	u, err := s.users.GetByID(ctx, *userID)
	if err != nil {
		return "", time.Time{}, apperror.Internal("failed to load user").Wrap(err)
	}
	if u == nil {
		return "", time.Time{}, apperror.BadRequest("link reset tidak valid atau sudah kedaluwarsa")
	}
	return s.issue(u)
}

// ChangePassword requires the current one. It returns no new session:
// the epoch bump signs every device out, including the one asking, and
// the dashboard sends the user back to the login screen.
func (s *AuthService) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return apperror.Internal("failed to load user").Wrap(err)
	}
	if u == nil || u.PasswordHash == nil {
		return apperror.BadRequest("kata sandi saat ini salah")
	}
	ok, err := VerifyPassword(*u.PasswordHash, current)
	if err != nil && !errors.Is(err, ErrBadHash) {
		return apperror.Internal("failed to verify password").Wrap(err)
	}
	if !ok {
		return apperror.BadRequest("kata sandi saat ini salah")
	}
	if err := ValidatePassword(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return apperror.Internal("failed to hash password").Wrap(err)
	}
	if err := s.users.SetPassword(ctx, userID, hash); err != nil {
		return apperror.Internal("failed to store password").Wrap(err)
	}
	return nil
}

// LogoutAll revokes every session this account has open.
func (s *AuthService) LogoutAll(ctx context.Context, userID uuid.UUID) error {
	if err := s.users.BumpEpoch(ctx, userID); err != nil {
		return apperror.Internal("failed to revoke sessions").Wrap(err)
	}
	return nil
}
