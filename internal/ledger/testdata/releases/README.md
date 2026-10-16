Ledgers in the shape each release wrote, built by hand from the `Record`
type at that release's tag, never taken from a real ledger. Every release
so far wrote schema 1 and only ever added fields: rc1 is the first shape,
rc5 added `transforms`, rc9 `limits`, and rc12 `quota`, `local_cause`, and
`handback`. `newer` holds a record from a schema this Janus does not know.
`TestEveryReleasedLedgerShapeIsRead` reads them all.
