package service

// DefaultCategory is one node of the starter tree a new account gets.
// Children are always essential: the only nesting the default tree has
// is under Tagihan, and a bill is essential by definition.
type DefaultCategory struct {
	Name      string
	Essential bool
	Children  []string
}

// DefaultCategories is the category tree every new account starts with,
// and the same tree cmd/seed builds its development dataset from — one
// definition, so a fresh signup and a seeded dev database agree.
var DefaultCategories = []DefaultCategory{
	{Name: "Makanan & Minuman", Essential: true},
	{Name: "Transportasi", Essential: true},
	{Name: "Tagihan", Essential: true, Children: []string{"Listrik", "Internet"}},
	{Name: "Belanja"},
	{Name: "Hiburan"},
	{Name: "Penghasilan"},
}

// DefaultSourceName is the one Source a new account starts with: cash
// is the only place money is guaranteed to live, and an empty Sources
// list would make the first capture impossible.
const DefaultSourceName = "Cash"
