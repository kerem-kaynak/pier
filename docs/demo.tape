# pier demo — render with: cd /tmp/pier-demo/shop && vhs ~/pier/docs/demo.tape
# The gif lands at ./demo.gif in the staged repo — copy it to pier's docs/.
# Run from the staged repo: the TUI create picks the repo up from the cwd.
#
# vhs drives a real terminal, so this records against real sessions.
# Stage before recording: a throwaway repo (e.g. /tmp/pier-demo/shop) whose
# .pier-setup.sh starts a dev server on :3000, then
#   pier checkout-flow -d --no-park
#   pier search-index  -d --no-park
# Both must be past creating when recording starts. The TUI sorts by name,
# so rows are: checkout-flow, payments-retry (created live), search-index.
# The default instance type should be t4g.medium so the resize beat grows
# to t4g.xlarge two picker rows down. All real waiting hides inside
# Hide/Show — never guess AWS round-trips with fixed visible sleeps.

Output demo.gif

Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 12
Set TypingSpeed 70ms
Set Framerate 24

# --- open the TUI: the hero shot -------------------------------------------
Type "pier"
Enter
Sleep 800ms
Hide
Wait+Screen@60s /checkout-flow\s+shop/
Show
Sleep 1.5s

# --- create a session from the TUI ------------------------------------------
Type "n"
Sleep 600ms
Type "payments-retry"
Sleep 400ms
Enter
# the status line + creating row register, then the real create (minutes on
# a stock image) hides until the row flips ready (● replaces the ◐)
Sleep 2.5s
Hide
Wait+Screen@900s /payments-retry\s+shop\s+●/
Show
Sleep 2s

# --- pier proxy: every session as a hostname --------------------------------
Type "q"
Sleep 300ms
Hide
Type "clear"
Enter
Show
Type "pier proxy"
Enter
Sleep 1.2s
# tunnels come up and the dev servers get discovered; all three mirror :3000
Hide
Wait+Screen@120s /checkout-flow\.pier ports: 3000/
Wait+Screen@120s /payments-retry\.pier ports: 3000/
Wait+Screen@120s /search-index\.pier ports: 3000/
Show
Sleep 3s
Ctrl+C
Sleep 1.2s

# --- back to the TUI for resize ---------------------------------------------
Hide
Type "clear"
Enter
Type "pier"
Enter
Wait+Screen@60s /search-index\s+shop\s+●/
Show
Sleep 1.5s

# cursor sits on checkout-flow (row 1): grow it from t4g.medium to t4g.xlarge
Type "m"
Sleep 1.5s
Down
Sleep 500ms
Down
Sleep 800ms
Enter
# the resize status line lands, then the real park+resume cycle (~40-60s)
# hides; the TUI stays open so the resize it drives runs to completion
Sleep 2.5s
Hide
Sleep 180s
Show
Sleep 1s

# --- delete a session --------------------------------------------------------
Down
Sleep 400ms
Down
Sleep 600ms
Type "d"
Sleep 1.5s
Type "y"
Sleep 1.2s
Hide
Sleep 25s
Show
Sleep 2.5s
