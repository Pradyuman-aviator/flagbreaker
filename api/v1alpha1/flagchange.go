package v1alpha1

// IsSwitchOn reports whether undoing the change includes switching the flag
// off: it switched the flag on, or came while a switch-on was a suspect. A
// change recorded before SwitchedOn existed is a switch-on when it left the
// flag on and changed no value.
func (c *FlagChange) IsSwitchOn() bool {
	return c.SwitchedOn || (c.Enabled && !c.ValueChanged)
}

// IsSuspect reports whether the change can be blamed for a breakage: it
// switched the flag on, set a new value, or both.
func (c *FlagChange) IsSuspect() bool {
	return c.IsSwitchOn() || c.ValueChanged
}
