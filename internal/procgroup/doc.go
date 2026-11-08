// Package procgroup gives a ccwrap launch on Windows a console process group
// of its own. A console sends Ctrl+C either to every process on it, the
// shell and any script that started ccwrap among them, or to one process
// group: in a group of its own, a session's Claude Code can be asked to exit
// by ccwrap stop while the rest of the console carries on. Elsewhere ccwrap
// stop signals Claude Code directly, and the package does nothing.
package procgroup
