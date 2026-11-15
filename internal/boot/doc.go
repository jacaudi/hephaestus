// Package boot decides what /boot does with a machine and renders the iPXE
// scripts that carry the answer. Both are pure: Decide applies the ten rows of
// design section 5.2 to a snapshot of facts, and Render turns a Decision into
// script text. The caller owns every side effect (transitions, events, tickets).
package boot
