// Package fleet holds the machines of the fleet: the lifecycle state machine
// of design section 5.2, the identity rules that normalize and classify the
// UUIDs and MAC addresses machines report, and the Repository that stores
// machines, their boot events and the audit log. The state machine and the
// identity rules are pure; only the Repository does I/O.
package fleet
