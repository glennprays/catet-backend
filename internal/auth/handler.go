package auth

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/gofiber/fiber/v2"
)

// Handler serves the auth surface: login, the OTP errands, signup from
// an invite, password reset and change, and the session's own account.
type Handler struct {
	svc *AuthService
}

func NewHandler(svc *AuthService) *Handler {
	return &Handler{svc: svc}
}

// Login exchanges phone + password for a session token. The Next.js BFF
// turns the token into the httpOnly session cookie.
func (h *Handler) Login(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.LoginRequest](c)
	if err != nil {
		return err
	}
	token, expiresAt, err := h.svc.Login(c.UserContext(), req.Phone, req.Password, c.IP())
	if err != nil {
		return err
	}
	return httperror.OK(c, response.SessionData{Token: token, ExpiresAt: expiresAt})
}

// RequestOtp dispatches a WhatsApp code. The answer is the same whether
// the number is registered, invited or unknown — only the first two
// actually receive anything.
func (h *Handler) RequestOtp(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.OtpRequest](c)
	if err != nil {
		return err
	}
	if err := h.svc.RequestCode(c.UserContext(), req.Phone, c.IP()); err != nil {
		return err
	}
	return httperror.OK(c, response.OtpRequestData{Sent: true})
}

// InviteInfo tells the register page which number the link is for.
func (h *Handler) InviteInfo(c *fiber.Ctx) error {
	inv, err := h.svc.InviteInfo(c.UserContext(), c.Params("token"))
	if err != nil {
		return err
	}
	return httperror.OK(c, response.InviteInfoData{Phone: inv.Phone, ExpiresAt: inv.ExpiresAt})
}

// Register creates the account behind an invite and logs it in.
func (h *Handler) Register(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.RegisterRequest](c)
	if err != nil {
		return err
	}
	token, expiresAt, err := h.svc.Register(
		c.UserContext(), req.InviteToken, req.Code, req.DisplayName, req.Password)
	if err != nil {
		return err
	}
	return httperror.OK(c, response.SessionData{Token: token, ExpiresAt: expiresAt})
}

// VerifyReset trades a WhatsApp code for a short-lived reset token.
func (h *Handler) VerifyReset(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.ResetVerifyRequest](c)
	if err != nil {
		return err
	}
	token, expiresAt, err := h.svc.VerifyResetCode(c.UserContext(), req.Phone, req.Code, c.IP())
	if err != nil {
		return err
	}
	return httperror.OK(c, response.ResetTokenData{ResetToken: token, ExpiresAt: expiresAt})
}

// CompleteReset sets the new password and returns a session: the number
// was just proved, so a login screen would add nothing.
func (h *Handler) CompleteReset(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.ResetCompleteRequest](c)
	if err != nil {
		return err
	}
	token, expiresAt, err := h.svc.CompleteReset(c.UserContext(), req.ResetToken, req.Password)
	if err != nil {
		return err
	}
	return httperror.OK(c, response.SessionData{Token: token, ExpiresAt: expiresAt})
}

// Me describes the session's own account.
func (h *Handler) Me(c *fiber.Ctx) error {
	user := CurrentUser(c)
	return httperror.OK(c, response.MeData{
		ID:          user.ID,
		Phone:       user.Phone,
		DisplayName: user.DisplayName,
		IsAdmin:     user.IsAdmin,
	})
}

// ChangePassword requires the current password. It returns no new
// session: the epoch bump logs every browser out, this one included.
func (h *Handler) ChangePassword(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.ChangePasswordRequest](c)
	if err != nil {
		return err
	}
	if err := h.svc.ChangePassword(
		c.UserContext(), CurrentUser(c).ID, req.CurrentPassword, req.NewPassword); err != nil {
		return err
	}
	return httperror.OK(c, response.OKData{OK: true})
}

// LogoutAll revokes every session of the calling account.
func (h *Handler) LogoutAll(c *fiber.Ctx) error {
	if err := h.svc.LogoutAll(c.UserContext(), CurrentUser(c).ID); err != nil {
		return err
	}
	return httperror.OK(c, response.OKData{OK: true})
}
