# License exceptions

Go modules that the isshoni binary may link although their license is not on the shipped allowlist
(`docs/m1/06-deploy-and-ci.md` §8.3). `tools/notices` (`task notices`) fails for such a module unless it has a row
here; go-licenses (`task licenses`) already lets MPL-2.0 through as "reciprocal", so this table is where the reason
is written down.

Only file-level copyleft licenses such as MPL-2.0 qualify, for a module used unmodified. GPL, AGPL, LGPL, SSPL and
BUSL never do: the tool refuses such a row.

One row per module and license: the module path in backquotes, the license id that licensecheck reports for the
module (the tool's error message names it), and the reason: what the module does for isshoni, that it is used
unmodified, and where its source is. `task notices` warns about a row that matches no linked module; remove it then.

| Module | License | Reason |
|---|---|---|
