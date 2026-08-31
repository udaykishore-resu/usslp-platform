package domain

import "github.com/usslp/usslp/platform/pkg/canon"

// LeafOTAResult is the upstream counterpart of canon.LeafOTA.
//
// Interface contract §3 defines the downstream `…/sec/{sec}/labels/{label}/ota`
// topic for the trigger, and lists `…/ack` upstream for delivery
// acknowledgements of *price* updates. A firmware result needs its own leaf
// rather than sharing that one, for two reasons. It is a different shape — a
// version, a duration and a battery reading rather than a sequence number and a
// refresh time — and it is a different consumer: the Label Service's ACK
// pipeline is on the three-second price path and would have to filter out
// firmware results at a rate it cannot afford. Publishing them under the OTA
// leaf keeps each subscriber's stream homogeneous.
//
// The leaf sits under the existing per-label zone namespace, so it inherits the
// same tenant ACL and the same routing as everything else the controller sends.
const LeafOTAResult = "ota/result"

// FilterAllOTAResults is the cloud-side subscription for firmware results from
// every store.
//
// Like the canon.FilterAll* constants it begins with a tenant wildcard: a cloud
// service is authorised across every tenant, where a device's credential is
// confined to its own.
const FilterAllOTAResults = canon.MQTTRoot + "/+/+/+/sec/+/labels/+/" + LeafOTAResult
