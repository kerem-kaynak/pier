# pier demo — render with: vhs docs/demo.tape   (https://github.com/charmbracelet/vhs)
#
# vhs drives a real terminal, so this records against real sessions.
# Stage before recording:
#   - two or three live sessions with believable names, e.g.
#     checkout-flow (working), payments-retry (parked), search-index (idle)
#   - the parked one resumes during the Hide block below
# Target: ~12s of GIF. All real waiting happens inside Hide/Show.

Output docs/demo.gif

Set FontSize 15
Set Width 1100
Set Height 620
Set Padding 12
Set TypingSpeed 70ms
Set Framerate 24

# --- open the TUI: the hero shot -------------------------------------------
Type "pier"
Enter
Sleep 3s

# --- pick the parked session and attach ------------------------------------
Down
Sleep 800ms
Enter

# resume takes ~20-30s; skip it in the recording
Hide
Sleep 35s
Show

# inside the session: tmux, agent on its branch
Sleep 3.5s

# --- detach; the session will park itself ----------------------------------
Ctrl+B
Type "d"
Sleep 500ms

# back in the TUI: states visible, one row parked at ~$3-4/mo
Sleep 2.5s

Type "q"
Sleep 1s
