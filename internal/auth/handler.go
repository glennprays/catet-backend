package auth

import (
	"github.com/glennprays/catet-backend/internal/httperror"
	"github.com/glennprays/catet-backend/internal/params/request"
	"github.com/glennprays/catet-backend/internal/params/response"
	"github.com/gofiber/fiber/v2"
)

// Handler serves /auth/otp/request, /auth/otp/verify and /auth/me.
type Handler struct {
	otp    *OtpService
	tokens *TokenService
}

func NewHandler(otp *OtpService, tokens *TokenService) *Handler {
	return &Handler{otp: otp, tokens: tokens}
}

// RequestOtp validates the phone and dispatches a code.
func (h *Handler) RequestOtp(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.OtpRequest](c)
	if err != nil {
		return err
	}
	if err := h.otp.Request(c.UserContext(), req.Phone); err != nil {
		return err
	}
	return httperror.OK(c, response.OtpRequestData{Sent: true})
}

// VerifyOtp exchanges a code for a session token. The Next.js BFF
// turns the token into the httpOnly session cookie.
func (h *Handler) VerifyOtp(c *fiber.Ctx) error {
	req, err := httperror.BindAndValidate[request.OtpVerifyRequest](c)
	if err != nil {
		return err
	}
	token, expiresAt, err := h.otp.Verify(c.UserContext(), req.Phone, req.Code)
	if err != nil {
		return err
	}
	return httperror.OK(c, response.OtpVerifyData{Token: token, ExpiresAt: expiresAt})
}

// Me echoes the session subject phone.
func (h *Handler) Me(c *fiber.Ctx) error {
	return httperror.OK(c, response.MeData{Phone: Phone(c)})
}
