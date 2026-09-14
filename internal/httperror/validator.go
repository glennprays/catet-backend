package httperror

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/glennprays/catet-backend/internal/apperror"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
)

// validate is shared across BindAndValidate calls; the library is
// goroutine-safe. It's configured to report JSON field names (rather
// than Go struct field names) in validation errors so clients see
// the same identifiers they sent. Custom rules cover the date-ish
// string params the API accepts (Asia/Jakarta calendar dates).
var validate = func() *validator.Validate {
	v := validator.New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})

	mustRegister(v, "yyyy-mm-date", func(s string) bool {
		_, err := time.Parse("2006-01-02", s)
		return err == nil
	})
	mustRegister(v, "yyyy-mm", func(s string) bool {
		_, err := time.Parse("2006-01", s)
		return err == nil
	})

	return v
}()

// mustRegister wires a string-rule validator and panics on a name
// clash — a programming error caught at package init, not at runtime.
func mustRegister(v *validator.Validate, tag string, fn func(string) bool) {
	if err := v.RegisterValidation(tag, func(fl validator.FieldLevel) bool {
		s, ok := fl.Field().Interface().(string)
		return ok && fn(s)
	}); err != nil {
		panic(fmt.Sprintf("register validator %q: %v", tag, err))
	}
}

// BindAndValidate parses the request body into T and validates it
// against its struct tags. On failure it returns a typed *apperror
// that the global error handler will turn into a proper response:
//
//   - parse error              -> 400 BAD_REQUEST
//   - validation failures      -> 400 VALIDATION_FAILED with details[]
//
// Handlers can use it as:
//
//	dto, err := httperror.BindAndValidate[CreateUserDTO](c)
//	if err != nil {
//	    return err
//	}
func BindAndValidate[T any](c *fiber.Ctx) (T, error) {
	var dto T
	if err := c.BodyParser(&dto); err != nil {
		return dto, apperror.BadRequestf("invalid request body: %v", err)
	}
	if err := validate.Struct(&dto); err != nil {
		return dto, fromValidationError(err)
	}
	return dto, nil
}

// fromValidationError converts go-playground/validator failures into an
// *apperror.Error carrying one FieldError per failing field.
func fromValidationError(err error) *apperror.Error {
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return apperror.BadRequest(err.Error())
	}
	details := make([]apperror.FieldError, 0, len(ve))
	for _, fe := range ve {
		details = append(details, apperror.FieldError{
			Field:   strings.ToLower(stripRootNamespace(fe.Namespace())),
			Rule:    fe.Tag(),
			Message: ruleMessage(fe),
		})
	}
	out := apperror.Validation("request validation failed")
	out.Details = details
	return out
}

// stripRootNamespace removes the top-level struct name from a
// validator namespace, e.g. "CreateUserDTO.email" -> "email".
func stripRootNamespace(ns string) string {
	if _, rest, ok := strings.Cut(ns, "."); ok {
		return rest
	}
	return ns
}

// ruleMessage produces a short human-readable message for the failed
// rule. Kept deliberately generic — callers wanting custom messages
// can post-process the Details slice.
func ruleMessage(fe validator.FieldError) string {
	if p := fe.Param(); p != "" {
		return fmt.Sprintf("failed on '%s=%s' rule", fe.Tag(), p)
	}
	return fmt.Sprintf("failed on '%s' rule", fe.Tag())
}
