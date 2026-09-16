package handler

import (
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/glennprays/catet-backend/internal/auth"
	"github.com/glennprays/catet-backend/internal/domain/entity"
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/glennprays/catet-backend/internal/repository"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// UserHandler serves the admin-only account management surface:
// Settings → Pengguna. Every route below is mounted behind
// auth.RequireAdmin.
type UserHandler struct {
	users   *repository.UserRepository
	invites *auth.InviteService
}

func NewUserHandler(users *repository.UserRepository, invites *auth.InviteService) *UserHandler {
	return &UserHandler{users: users, invites: invites}
}

func (h *UserHandler) List(c *fiber.Ctx) error {
	users, err := h.users.List(c.UserContext())
	if err != nil {
		return apperror.Internal("failed to list users").Wrap(err)
	}
	out := make([]response.UserData, 0, len(users))
	for i := range users {
		out = append(out, userData(&users[i]))
	}
	return httperror.OK(c, response.UsersData{Users: out})
}

// CreateInvite issues an invite and sends the register link over
// WhatsApp. The token is deliberately dropped here: the admin page must
// not be able to show a link that bypasses the number.
func (h *UserHandler) CreateInvite(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.InviteRequest](c)
	if err != nil {
		return err
	}
	inv, _, err := h.invites.Issue(c.UserContext(), req.Phone, auth.CurrentUser(c).ID)
	if err != nil {
		return err
	}
	return httperror.OK(c, inviteData(inv))
}

func (h *UserHandler) ListInvites(c *fiber.Ctx) error {
	invites, err := h.invites.ListOpen(c.UserContext())
	if err != nil {
		return err
	}
	out := make([]response.InviteData, 0, len(invites))
	for i := range invites {
		out = append(out, inviteData(&invites[i]))
	}
	return httperror.OK(c, response.InvitesData{Invites: out})
}

func (h *UserHandler) RevokeInvite(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.invites.Revoke(c.UserContext(), id); err != nil {
		return err
	}
	return httperror.OK(c, response.OKData{OK: true})
}

// Deactivate soft-disables an account and kills its sessions. Refused
// for the caller's own account: an admin who locks themselves out has
// no way back except the operator CLI.
func (h *UserHandler) Deactivate(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	if id == auth.CurrentUser(c).ID {
		return apperror.BadRequest("cannot deactivate yourself")
	}
	now := time.Now().UTC()
	return h.setDeactivated(c, id, &now)
}

func (h *UserHandler) Activate(c *fiber.Ctx) error {
	id, err := uuidParam(c, "id")
	if err != nil {
		return err
	}
	return h.setDeactivated(c, id, nil)
}

func (h *UserHandler) setDeactivated(c *fiber.Ctx, id uuid.UUID, at *time.Time) error {
	user, err := h.users.GetByID(c.UserContext(), id)
	if err != nil {
		return apperror.Internal("failed to load user").Wrap(err)
	}
	if user == nil {
		return apperror.NotFound("pengguna tidak ditemukan")
	}
	if err := h.users.SetDeactivated(c.UserContext(), id, at); err != nil {
		return apperror.Internal("failed to update user").Wrap(err)
	}
	updated, err := h.users.GetByID(c.UserContext(), id)
	if err != nil {
		return apperror.Internal("failed to load user").Wrap(err)
	}
	return httperror.OK(c, userData(updated))
}

func userData(u *entity.User) response.UserData {
	return response.UserData{
		ID:            u.ID,
		Phone:         u.Phone,
		DisplayName:   u.DisplayName,
		IsAdmin:       u.IsAdmin,
		DeactivatedAt: u.DeactivatedAt,
		LastLoginAt:   u.LastLoginAt,
		CreatedAt:     u.CreatedAt,
	}
}

func inviteData(i *entity.Invite) response.InviteData {
	return response.InviteData{
		ID:        i.ID,
		Phone:     i.Phone,
		ExpiresAt: i.ExpiresAt,
		CreatedAt: i.CreatedAt,
	}
}
