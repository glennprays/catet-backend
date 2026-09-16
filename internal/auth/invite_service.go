package auth

import (
	"context"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/bot/tmpl"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/gateway"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/glennprays/log"
	waga "github.com/glennprays/whatsapp-gateway-sdk-go"
	"github.com/google/uuid"
)

// inviteTTL is how long a register link stays usable. A week is long
// enough that the invitee can act on their own time and short enough
// that a forwarded link goes stale.
const inviteTTL = 7 * 24 * time.Hour

// InviteService issues registration invites and delivers them over
// WhatsApp. Delivery over the same channel the product lives in is the
// second factor: the link alone is useless without the number, and the
// number is what the invite is bound to.
type InviteService struct {
	invites      *repository.InviteRepository
	users        *repository.UserRepository
	gw           gateway.Sender
	log          *log.Logger
	dashboardURL string
	now          func() time.Time
}

func NewInviteService(
	invites *repository.InviteRepository,
	users *repository.UserRepository,
	gw gateway.Sender,
	logger *log.Logger,
	dashboardURL string,
) *InviteService {
	return &InviteService{
		invites:      invites,
		users:        users,
		gw:           gw,
		log:          logger.With(log.String("component", "invite")),
		dashboardURL: dashboardURL,
		now:          time.Now,
	}
}

// Issue mints an invite for a number and sends it the register link. The
// plaintext token is returned as well as stored-hashed: the operator CLI
// prints it for the case where the gateway is down, and the HTTP handler
// throws it away.
//
// A failed send is not a failed invite. The row is the authority — the
// admin page lists it, and the bot re-sends the link the first time the
// invitee writes in.
func (s *InviteService) Issue(ctx context.Context, phone string, createdBy uuid.UUID) (*entity.Invite, string, error) {
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return nil, "", err
	}
	existing, err := s.users.GetByPhone(ctx, normalized)
	if err != nil {
		return nil, "", apperror.Internal("failed to look up user").Wrap(err)
	}
	if existing != nil {
		return nil, "", apperror.Conflict("nomor sudah terdaftar")
	}

	token, err := randomToken()
	if err != nil {
		return nil, "", apperror.Internal("failed to generate invite token").Wrap(err)
	}
	inv, err := s.invites.Create(ctx, normalized, HashToken(token), createdBy, s.now().Add(inviteTTL))
	if err != nil {
		return nil, "", apperror.Internal("failed to store invite").Wrap(err)
	}

	text := tmpl.Render(tmpl.LangID, "invite_sent", map[string]any{"URL": s.RegisterURL(token)})
	chat := waga.FormatMSISDN(gateway.DigitsOnly(normalized))
	if _, err := s.gw.SendText(ctx, chat, text, "invite:"+inv.ID.String()); err != nil {
		s.log.Warn("invite", "invite link not delivered", map[string]any{
			"invite_id": inv.ID.String(), "error": err.Error(),
		})
	}
	return inv, token, nil
}

// Reissue is the bot's path: an unknown sender who was invited gets a
// fresh link instead of silence. The stored hash cannot be turned back
// into a link, so "resend" means "invite again" — the open row is
// replaced and a new token goes out. false means the number has no open
// invite and stays a silent drop.
func (s *InviteService) Reissue(ctx context.Context, phoneDigits string) (bool, error) {
	inv, err := s.invites.OpenByPhoneDigits(ctx, phoneDigits)
	if err != nil {
		return false, err
	}
	if inv == nil {
		return false, nil
	}
	if _, _, err := s.Issue(ctx, inv.Phone, inv.CreatedBy); err != nil {
		return true, err
	}
	return true, nil
}

// ListOpen lists invites still waiting to be accepted.
func (s *InviteService) ListOpen(ctx context.Context) ([]entity.Invite, error) {
	out, err := s.invites.ListOpen(ctx)
	if err != nil {
		return nil, apperror.Internal("failed to list invites").Wrap(err)
	}
	return out, nil
}

// Revoke withdraws an invite that has not been used.
func (s *InviteService) Revoke(ctx context.Context, id uuid.UUID) error {
	deleted, err := s.invites.Delete(ctx, id)
	if err != nil {
		return apperror.Internal("failed to revoke invite").Wrap(err)
	}
	if !deleted {
		return apperror.NotFound("undangan tidak ditemukan")
	}
	return nil
}

// RegisterURL is the link the invitee opens. It is the only place the
// plaintext token appears.
func (s *InviteService) RegisterURL(token string) string {
	return s.dashboardURL + "/register?invite=" + token
}
