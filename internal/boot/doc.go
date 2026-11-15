// Package boot decides what /boot does with a machine and renders the iPXE
// scripts that carry the answer. Decide and Render stay pure: Decide applies
// the ten rows of design section 5.2 to a snapshot of facts, and Render turns a
// Decision into script text. Handlers serves /start, /boot, /tftp and /discover
// behind RequireSubnet and applies the side effects (transitions, events,
// reports, tickets).
package boot
