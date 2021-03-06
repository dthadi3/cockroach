// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package batcheval

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/spanset"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

func init() {
	RegisterReadWriteCommand(roachpb.TransferLease, declareKeysTransferLease, TransferLease)
}

func declareKeysTransferLease(
	rs ImmutableRangeState, _ roachpb.Header, _ roachpb.Request, latchSpans, _ *spanset.SpanSet,
) {
	// TransferLease must not run concurrently with any other request so it uses
	// latches to synchronize with all other reads and writes on the outgoing
	// leaseholder. Additionally, it observes the state of the timestamp cache
	// and so it uses latches to wait for all in-flight requests to complete.
	//
	// Because of this, it declares a non-MVCC write over every addressable key
	// in the range, even through the only key the TransferLease actually writes
	// to is the RangeLeaseKey. This guarantees that it conflicts with any other
	// request because every request must declare at least one addressable key.
	//
	// We could, in principle, declare these latches as MVCC writes at the time
	// of the new lease. Doing so would block all concurrent writes but would
	// allow reads below the new lease timestamp through. However, doing so
	// would only be safe if we also accounted for clock uncertainty in all read
	// latches so that any read that may need to observe state on the new
	// leaseholder gets blocked. We actually already do this for transactional
	// reads (see DefaultDeclareIsolatedKeys), but not for non-transactional
	// reads. We'd need to be careful here, so we should only pull on this if we
	// decide that doing so is important.
	declareAllKeys(latchSpans)
}

// TransferLease sets the lease holder for the range.
// Unlike with RequestLease(), the new lease is allowed to overlap the old one,
// the contract being that the transfer must have been initiated by the (soon
// ex-) lease holder which must have dropped all of its lease holder powers
// before proposing.
func TransferLease(
	ctx context.Context, readWriter storage.ReadWriter, cArgs CommandArgs, resp roachpb.Response,
) (result.Result, error) {
	// When returning an error from this method, must always return
	// a newFailedLeaseTrigger() to satisfy stats.
	args := cArgs.Args.(*roachpb.TransferLeaseRequest)

	// NOTE: we use the range's current lease as prevLease instead of
	// args.PrevLease so that we can detect lease transfers that will
	// inevitably fail early and reject them with a detailed
	// LeaseRejectedError before going through Raft.
	prevLease, _ := cArgs.EvalCtx.GetLease()

	// Forward the lease's start time to a current clock reading. At this
	// point, we're holding latches across the entire range, we know that
	// this time is greater than the timestamps at which any request was
	// serviced by the leaseholder before it stopped serving requests (i.e.
	// before the TransferLease request acquired latches).
	newLease := args.Lease
	newLease.Start.Forward(cArgs.EvalCtx.Clock().NowAsClockTimestamp())
	args.Lease = roachpb.Lease{} // prevent accidental use below

	// If this check is removed at some point, the filtering of learners on the
	// sending side would have to be removed as well.
	if err := roachpb.CheckCanReceiveLease(newLease.Replica, cArgs.EvalCtx.Desc()); err != nil {
		return newFailedLeaseTrigger(true /* isTransfer */), err
	}

	log.VEventf(ctx, 2, "lease transfer: prev lease: %+v, new lease: %+v", prevLease, newLease)
	return evalNewLease(ctx, cArgs.EvalCtx, readWriter, cArgs.Stats,
		newLease, prevLease, false /* isExtension */, true /* isTransfer */)
}
