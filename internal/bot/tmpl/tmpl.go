// Package tmpl renders every user-visible bot reply. All copy lives in
// templates/bot/<lang>.tmpl so no Go code concatenates user-facing text
// and a translator can work without touching the harness. Money must
// arrive already formatted by internal/money — the templates never add
// currency marks of their own.
package tmpl

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/glennprays/catet-backend/templates"
)

// LangID is the fallback language: Indonesian is the primary user
// language, so an unrecognised code still gets a usable reply.
const LangID = "id"

// LangEN is the supported second language.
const LangEN = "en"

// names is the contract with the harness: every one of these renders in
// every language, checked at init so a missing translation fails the
// process at boot instead of silently sending an empty message.
var names = []string{
	"recorded_one",
	"recorded_income",
	"recorded_transfer",
	"recorded_many",
	"uncategorized",
	"hold_saved",
	"hold_to_dashboard",
	"q_amount",
	"q_amount_roles",
	"q_source",
	"q_destination",
	"q_refund_target",
	"refund_done",
	"refund_no_target",
	"batal_done",
	"batal_nothing",
	"batal_bad_index",
	"discarded",
	"no_open_question",
	"open_question_reminder",
	"unknown",
	"corrected",
	"correct_nothing",
	"correct_failed",
	"balance_one",
	"balance_all",
	"unsupported_media",
	"photo_unreadable",
	"photo_no_total",
	"photo_not_receipt",
	"photo_vision_down",
	"photo_statement",
	"photo_currency",
	"photo_too_large",
	"photo_duplicate",
	"photo_llm_off",
	"receipt_summary",
	"receipt_hold",
	"receipt_recorded",
	"receipt_updated",
	"receipt_need_source",
	"receipt_default_learned",
	"receipt_split_mismatch",
	"receipt_split_unknown_source",
	"receipt_change_source",
	"receipt_change_split",
	"receipt_change_category",
	"receipt_change_merchant",
	"receipt_change_date",
	"rekap_hari",
	"rekap_minggu",
	"bill_reminder",
	"lunas_none",
	"lunas_choose",
	"lunas_amount",
	"lunas_source",
	"lunas_done",
	"lunas_completed",
	"invite_sent",
	"error_generic",
}

var sets map[string]*template.Template

func init() {
	funcs := template.FuncMap{"join": join}
	sets = make(map[string]*template.Template, 2)
	for _, lang := range []string{LangID, LangEN} {
		file := "bot/" + lang + ".tmpl"
		t, err := template.New(lang).Funcs(funcs).ParseFS(templates.FS, file)
		if err != nil {
			panic(fmt.Sprintf("tmpl: parse %s: %v", file, err))
		}
		for _, name := range names {
			if t.Lookup(name) == nil {
				panic(fmt.Sprintf("tmpl: %s is missing template %q", file, name))
			}
		}
		sets[lang] = t
	}
}

// Render renders the named template in lang, falling back to Indonesian
// for any unknown code. A failure returns the empty string: this is a
// leaf package with no logger, and a Go error text must never reach a
// WhatsApp reply. The caller decides what to send instead.
func Render(lang, name string, data any) string {
	t, ok := sets[lang]
	if !ok {
		t = sets[LangID]
	}
	var b strings.Builder
	if err := t.ExecuteTemplate(&b, name, data); err != nil {
		return ""
	}
	return strings.TrimSpace(b.String())
}

// Names lists the template names this package guarantees in every
// language.
func Names() []string {
	out := make([]string, len(names))
	copy(out, names)
	return out
}

// join renders a list as one human-readable run; templates use it for
// the source pick-lists and anomaly days.
func join(items []string, sep string) string {
	return strings.Join(items, sep)
}
