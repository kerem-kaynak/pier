# pier demo — render with: vhs docs/demo.tape   (https://github.com/charmbracelet/vhs)
#
# vhs drives a real terminal, so this records against real sessions.
# Stage before recording (throwaway repo, e.g. /tmp/pier-demo/shop):
#   pier checkout-flow -d --no-park
#   pier search-index  -d --no-park
#   pier payments-retry -d --idle 2m     # parked by recording time
# payments-retry must be PARKED when recording starts, and must sit on the
# third TUI row (check `pier ls` order; adjust the Down presses if not).
# Target: ~13s of GIF. All real waiting happens inside Hide/Show.

Output docs/demo.gif

Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 12
Set TypingSpeed 70ms
Set Framerate 24

# --- open the TUI: the hero shot -------------------------------------------
# The session list takes ~5s to fetch (describe-instances + quota); key
# presses before it loads are swallowed and Enter lands on row 1. Hold long
# enough for the loaded table to be the hero shot.
Type "pier"
Enter
Sleep 7s

# --- pick the parked session ------------------------------------------------
Down
Sleep 400ms
Down
Sleep 700ms
Enter

# the TUI hands off to attach; catch the dim "resuming (~20-30s)" line...
Sleep 1.2s
# ...then skip the actual resume wait
Hide
Sleep 50s
Show

# inside: tmux at agent@payments-retry:~/work/shop, repo on its branch
Sleep 1s
Type "git status"
Enter
Sleep 2.2s

# --- detach; the session parks itself --------------------------------------
Ctrl+B
Type "d"
Sleep 1s

# skip the 2m idle window plus the EC2 stop itself, then the payoff:
# parked at disk-only cost (clear first — the dead TUI scrollback is noise)
Hide
Sleep 210s
Type "clear"
Enter
Show
Type "pier ls"
Enter
# ls makes real AWS calls (~2-3s before output); hold long enough to read it
Sleep 5.5s
