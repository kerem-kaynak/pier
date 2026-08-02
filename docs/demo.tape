# pier demo — render with: vhs docs/demo.tape   (https://github.com/charmbracelet/vhs)
#
# vhs drives a real terminal, so this records against real sessions.
# Stage before recording (throwaway repo, e.g. /tmp/pier-demo/shop):
#   pier checkout-flow -d --no-park
#   pier search-index  -d --no-park
#   pier payments-retry -d --idle 2m     # parked by recording time
# payments-retry must be PARKED when recording starts, and must sit on the
# third TUI row (check `pier ls` order; adjust the Down presses if not).
# The claude beat assumes claude is authenticated in sessions (secrets
# manifest or claude_oauth_token in the pier config).
# All real waiting happens inside Hide/Show.

Output docs/demo.gif

Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 12
Set TypingSpeed 70ms
Set Framerate 24

# --- open the TUI: the hero shot -------------------------------------------
# The session fetch rides real AWS round-trips (2-15s). Key presses before
# the list loads are swallowed and Enter lands on row 1 — never guess the
# fetch time with a fixed sleep; wait for the parked row to render.
Type "pier"
Enter
Sleep 1s
Hide
Wait+Screen@60s /parked/
Show
Sleep 3s

# --- pick the parked session ------------------------------------------------
Down
Sleep 400ms
Down
Sleep 700ms
Enter

# the TUI hands off to attach; catch the dim "resuming (~20-30s)" line...
Sleep 1.2s
# ...then skip the actual resume, however long it takes: the shell prompt
# only ever renders once tmux is attached
Hide
Wait+Screen@180s /agent@payments-retry/
Show

# inside: tmux at agent@payments-retry:~/work/shop, repo on its branch
Sleep 1s
Type "git status"
Enter
Sleep 2s

# --- talk to the agent -------------------------------------------------------
# The prompt names a file on purpose: Read/Grep are auto-allowed, so the
# answer needs no permission dialog. Open-ended prompts make claude reach for
# Bash (git show, ls) and the take stalls on an approval prompt.
Type "claude"
Enter
Sleep 3s
Type@40ms "read src/routes.js: how do retries handle timeouts? answer in one sentence"
Sleep 500ms
Enter
# let the thinking spinner register, then skip the real wait (~25s measured;
# done when the spinner verb flips to past tense: "Cooked for 11s")
Sleep 1.5s
Hide
Wait+Screen@90s /ed for \d+s/
Show
# the answer, on screen long enough to read
Sleep 4.5s

# --- detach; the session parks itself --------------------------------------
# claude stays open on purpose: an idle REPL is ~0% CPU, so the supervisor
# still counts the session idle and parks it — that's the story.
Ctrl+B
Type "d"
Sleep 1s

# skip the 2m idle window plus the EC2 stop itself, then the payoff:
# parked at disk-only cost (clear first — the dead TUI scrollback is noise)
Hide
Sleep 240s
Type "clear"
Enter
Show
Type "pier ls"
Enter
Sleep 300ms
# ls latency rides real AWS round-trips (2-8s): hide the wait and reveal the
# payoff the moment the parked row prints
Hide
Wait+Screen@30s /parked/
Show
Sleep 4.5s
