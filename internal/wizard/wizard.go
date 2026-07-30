// Package wizard implements `pier setup` — the one-time, sub-3-minute flow.
//
// Phases:
//  1. Detect  — aws/gcloud CLIs + profiles/projects, session-manager-plugin,
//     gh login, ~/.claude + ~/.codex configs. Everything found becomes a
//     prefilled default; a second dev on a prepared account sails through.
//  2. Ask     — <=6 questions: cloud(s), profile/region or project/zone,
//     sizes, parking policy (30m / custom / never), secrets-manifest confirm
//     (incl. offering `claude setup-token` for macOS Keychain subscription
//     auth), optional bake.
//  3. Apply   — driver.SetupOnce (idempotent; prints the exact resources
//     before touching anything; --print-admin emits them for an admin
//     instead when the dev lacks IAM rights).
//  4. Doctor  — quota headroom, attach plugin, connectivity; then writes
//     ~/.config/pier/config.toml and prints "cd <repo> && pier <branch>".
package wizard

import "errors"

var ErrNotImplemented = errors.New("wizard: not implemented yet")

// Run executes the four phases interactively.
func Run(printAdminOnly bool) error { return ErrNotImplemented }
