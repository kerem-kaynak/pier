# pier demo — render with: cd /tmp/pier-demo/shop && vhs ~/pier/docs/demo.tape
# The gif lands at ./demo.gif in the staged repo — copy it to pier's docs/.
# Run from the staged repo: the TUI create picks the repo up from the cwd.
#
# vhs drives a real terminal, so this records against real sessions.
# Stage before recording: a throwaway repo (e.g. /tmp/pier-demo/shop) whose
# .pier-setup.sh starts a dev server on :3000, then
#   pier checkout-flow -d --no-park
#   pier search-index  -d --no-park
# Both must be past creating (harness installed, claude authenticated —
# secrets manifest or claude_oauth_token) when recording starts. The TUI
# sorts by name, so rows are: checkout-flow, payments-retry (created live),
# search-index. The default instance type should be t4g.medium so the
# resize beat grows to t4g.xlarge two picker rows down. All real waiting
# hides inside Hide/Show — never guess AWS round-trips with visible sleeps.

Output demo.gif

Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 12
Set TypingSpeed 55ms
Set Framerate 24

# --- open the TUI: the hero shot -------------------------------------------
Type "pier"
Enter
Sleep 800ms
Hide
Wait+Screen@60s /checkout-flow\s+shop/
Show
Sleep 900ms

# --- create a session from the TUI ------------------------------------------
Type "n"
Sleep 500ms
Type "payments-retry"
Sleep 400ms
Enter
# the status line + creating row register, then the real create (minutes on
# a stock image) hides until the row flips ready (● replaces the ◐)
Sleep 1.8s
Hide
Wait+Screen@900s /payments-retry\s+shop\s+●/
Show
Sleep 800ms

# --- attach and talk to the agent -------------------------------------------
# cursor sits on checkout-flow (row 1); Enter hands off to ssh + tmux
Enter
Sleep 600ms
Hide
Wait+Screen@180s /agent@checkout-flow/
Show
Sleep 600ms
Type "claude"
Enter
# let the handoff register, then skip the splash settling
Sleep 800ms
Hide
Wait+Screen@60s /shortcuts/
Show
Sleep 300ms
# a file-scoped question on purpose: Read is auto-allowed, so the answer
# needs no permission dialog. Open-ended prompts make claude reach for Bash
# and the take stalls on an approval prompt.
Type@30ms "read src/routes.js: how do retries handle timeouts? one sentence"
Sleep 400ms
Enter
# the thinking spinner registers, then the real wait (~25s) hides; done when
# the spinner verb flips to past tense ("Cooked for 11s")
Sleep 800ms
Hide
Wait+Screen@120s /ed for \d+s/
Show
Sleep 3s

# --- detach: claude keeps running in the session's tmux ----------------------
Ctrl+B
Type "d"
Sleep 500ms

# --- pier proxy: every session as a hostname --------------------------------
Hide
Type "clear"
Enter
Show
Type "pier proxy"
Enter
Sleep 1s
# tunnels come up and the dev servers get discovered; all three mirror :3000
Hide
Wait+Screen@120s /checkout-flow\.pier ports: 3000/
Wait+Screen@120s /payments-retry\.pier ports: 3000/
Wait+Screen@120s /search-index\.pier ports: 3000/
Show
Sleep 2s
Ctrl+C
Sleep 600ms

# --- back to the TUI for resize ---------------------------------------------
Hide
Type "clear"
Enter
Type "pier"
Enter
Wait+Screen@60s /search-index\s+shop\s+●/
Show
Sleep 800ms

# cursor sits on checkout-flow (row 1): grow it from t4g.medium to t4g.xlarge
Type "m"
Sleep 900ms
Down
Sleep 350ms
Down
Sleep 500ms
Enter
# the resize status line lands, then the real park+resume cycle (~40-60s)
# hides; the TUI stays open so the resize it drives runs to completion
Sleep 1.5s
Hide
Sleep 180s
Show
Sleep 600ms

# --- delete a session --------------------------------------------------------
Down
Sleep 300ms
Down
Sleep 500ms
Type "d"
Sleep 900ms
Type "y"
Sleep 800ms
Hide
Sleep 25s
Show
Sleep 1.6s
