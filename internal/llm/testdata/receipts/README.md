# Receipt-reading fixtures

Scoring set for `TestReceiptEval` (build tag `llmeval`), the live accuracy
harness for whichever model `LLM_VISION_MODEL` names.

| file | what it proves |
|---|---|
| `000.jpg`, `001.jpg`, `010.jpg` | legible receipts: merchant, printed date and printed TOTAL must come back exactly |
| `000_dirty.jpg` | a blurred, rotated copy: the model must answer `legible:false`, never a guessed total |
| `010_top.jpg` | the top half of a long receipt, no TOTAL line: `has_total_line:false`, `total_minor:0` |

Source: ICDAR 2019 SROIE, research use. The three legible samples are
Malaysian receipts printed in MYR, so their `total_minor` is in sen — the
prompt's rule is "smallest unit of the printed currency", and the scorer is
currency-agnostic.

Adding a fixture is a JPEG plus one row in `expected.json`; that is how the
owner's own Indonesian receipts get scored before the model changes.
