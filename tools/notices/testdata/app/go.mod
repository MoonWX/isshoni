// A stand-in for the isshoni main module in the notices tests. Every dependency is a fixture module under
// ../mods, replaced by its directory, so the tests need no network and no module cache.
module example.com/app

go 1.26

require (
	example.com/apache v1.2.0
	example.com/bsd v0.3.0
	example.com/custom v0.1.0
	example.com/gpl v1.0.0
	example.com/mit v1.0.0
	example.com/mixed v1.0.0
	example.com/mpl v0.2.0
	example.com/nolicense v0.1.0
	example.com/winonly v1.1.0
)

replace (
	example.com/apache => ../mods/apache
	example.com/bsd => ../mods/bsd
	example.com/custom => ../mods/custom
	example.com/gpl => ../mods/gpl
	example.com/mit => ../mods/mit
	example.com/mixed => ../mods/mixed
	example.com/mpl => ../mods/mpl
	example.com/nolicense => ../mods/nolicense
	example.com/winonly => ../mods/winonly
)
