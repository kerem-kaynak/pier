# embedded assets

`make` cross-compiles `pier-supervisor-linux-{arm64,amd64}` into this
directory before building pier, which embeds them (`//go:embed assets`) and
pushes the right one into each session VM at create. The binaries are
gitignored; this file keeps the embed pattern valid on a fresh checkout.
