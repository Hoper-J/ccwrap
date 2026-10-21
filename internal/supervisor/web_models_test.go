package supervisor

import "testing"

// In the 4×2 ribbon (native TLS on, wider than 1024px) the Profile cell is in
// the first column, so its popover opens rightward; anchored to the cell's
// right edge it would run off the left of the window.
func TestProfilePopoverOpensRightwardIn4x2Ribbon(t *testing.T) {
	mustContainAllWeb(t, captureRenderedDashboardHTML(t),
		`@media (min-width:1025px){.ribbon:has(.ribbon-cell[data-ribbon="NATIVE TLS"]) .sp3-pop{left:0;right:auto}}`)
}
