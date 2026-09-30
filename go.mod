module github.com/MoonWX/isshoni

go 1.26

toolchain go1.27.1

// Node's dependency trees are not Go code: keep them out of ./... patterns.
ignore (
	./docs/node_modules
	./web/node_modules
)
