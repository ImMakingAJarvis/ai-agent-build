package kq

// DeleteRecords is the API jarvis-kq uses for `purge`. segmentio/kafka-go
// does not register the message, so we declare it here and register it in
// init() (protocol.Register panics on a duplicate ApiKey, and the library
// leaves 21 unregistered).
//
// Wire format v0/v1 (KIP-110; response framing [size][correlation][body],
// no api key/version in the response):
//   request:  topics ARRAY { name STRING,
//                            partitions ARRAY { partition_index INT32,
//                                               offset INT64,
//                                               timestamp INT64 } }
//   response: throttle_time_ms INT32,
//             topics ARRAY { name STRING,
//                            partitions ARRAY { partition_index INT32,
//                                               low_watermark INT64,
//                                               offset_error_code INT16,
//                                               offset_timestamp INT64 (v0 only) } }
// The request has no throttle field until v2 (which also moves to topic
// UUIDs); a stray leading INT32 would be parsed as a zero-length topic
// count and the broker would silently "succeed" on nothing. We declare
// max=v1 to stay on the classic encoding that every 3.x broker supports.

import (
	"io"

	"github.com/segmentio/kafka-go/protocol"
)

type deleteRecordsRequest struct {
	Topics []deleteRecordsRequestTopic `kafka:"min=v0,max=v1"`
}

type deleteRecordsRequestTopic struct {
	Name       string                        `kafka:"min=v0,max=v1"`
	Partitions []deleteRecordsRequestPartition `kafka:"min=v0,max=v1"`
}

type deleteRecordsRequestPartition struct {
	PartitionIndex int32 `kafka:"min=v0,max=v1"`
	Offset         int64 `kafka:"min=v0,max=v1"`
	Timestamp      int64 `kafka:"min=v0,max=v1"`
}

// ApiKey is DeleteRecords.
func (r *deleteRecordsRequest) ApiKey() protocol.ApiKey { return protocol.DeleteRecords }

// Every field needs an explicit kafka tag: an untagged field decodes with
// the version range [-1,-1], which matches no version, so it is silently
// dropped from every per-version decoder (the wire would then be misread,
// e.g. the throttle INT32 decoded as a zero topic count).
type deleteRecordsResponse struct {
	ThrottleTimeMs int32                    `kafka:"min=v0,max=v1"`
	Topics         []deleteRecordsResponseTopic `kafka:"min=v0,max=v1"`
}

type deleteRecordsResponseTopic struct {
	Name       string                       `kafka:"min=v0,max=v1"`
	Partitions []deleteRecordsResponsePartition `kafka:"min=v0,max=v1"`
}

type deleteRecordsResponsePartition struct {
	PartitionIndex  int32  `kafka:"min=v0,max=v1"`
	LowWatermark    int64  `kafka:"min=v0,max=v1"`
	OffsetErrorCode int16  `kafka:"min=v0,max=v1"`
	OffsetTimestamp int64  `kafka:"min=v0,max=v0"`
}

// ApiKey is DeleteRecords.
func (r *deleteRecordsResponse) ApiKey() protocol.ApiKey { return protocol.DeleteRecords }

func init() {
	protocol.Register(&deleteRecordsRequest{}, &deleteRecordsResponse{})
}

// protocolRoundTrip is protocol.RoundTrip behind a named function so the
// protocol import stays in this one file.
func protocolRoundTrip(rw io.ReadWriter, apiVersion int16, correlationID int32, clientID string, req protocol.Message) (protocol.Message, error) {
	return protocol.RoundTrip(rw, apiVersion, correlationID, clientID, req)
}
