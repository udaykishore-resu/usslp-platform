// Package domain is the Device Registry's pure model: device identity and
// lifecycle, the manufacturing manifest, the planogram, mesh topology and the
// health derivations built on top of them.
//
// It is the fleet's source of truth expressed without any infrastructure. There
// are no stores, no brokers, no HTTP handlers and no clocks here — every
// function that needs the time takes it as an argument. That is what makes the
// rules testable at the speed of a unit test and, more importantly, what keeps
// them in one place: "a label may not go from retired back to active" is a
// sentence in this package, not a condition duplicated across three adapters.
//
// # Why the lifecycle is a closed state machine
//
// A registry entry is not a row that services patch. Fifty million devices are
// touched by provisioning, by planogram uploads, by telemetry, by OTA and by
// human operators, all concurrently. If every one of those could assign any
// state, the fleet's state column would mean nothing within a week. Instead
// every change goes through [Device.Transition], which rejects an illegal move
// and returns the event the caller must publish. A device's history is then
// exactly the sequence of accepted transitions, and the read model is a
// projection of it rather than the truth itself.
//
// # Why events are values returned rather than published here
//
// The domain builds event payloads but never publishes them. A transition that
// succeeded in memory but whose event never reached the `device-events` stream
// would silently desynchronise the Label Service's fan-out directory, so the
// application layer is the only place allowed to decide the order of "persist,
// publish, apply" — and it is written so that a crash between those steps is
// recoverable rather than invisible.
package domain
