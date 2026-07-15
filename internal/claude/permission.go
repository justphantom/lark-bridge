package claude

// PermissionMode controls how the Claude Code CLI handles tool permission
// requests during a run. In -p (print / stream) mode the CLI is
// non-interactive, so a mode that never blocks on user input is required.
//
// These map 1:1 to the Claude Code CLI's --permission-mode flag values.
const (
	// PermissionModeAcceptEdits auto-accepts edits but surfaces other
	// permission-gated actions to the bridge's confirmation card.
	PermissionModeAcceptEdits = "acceptEdits"

	// PermissionModePlan runs in read-only planning mode; no mutations.
	PermissionModePlan = "plan"

	// PermissionModeBypassPermissions skips all permission checks. Most
	// permissive; use only when the working directory is disposable.
	PermissionModeBypassPermissions = "bypassPermissions"
)
