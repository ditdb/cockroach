// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package lease

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/redact"
)

// A lease stored in system.lease.
type storedLease struct {
	id         descpb.ID
	prefix     []byte
	version    int
	expiration tree.DTimestamp
	sessionID  []byte
}

func (s *storedLease) String() string {
	return redact.StringWithoutMarkers(s)
}

var _ redact.SafeFormatter = (*storedLease)(nil)

// SafeFormat implements redact.SafeFormatter.
func (s *storedLease) SafeFormat(w redact.SafePrinter, _ rune) {
	w.Printf("ID=%d ver=%d expiration=%s", s.id, s.version, s.expiration)
}

// descriptorVersionState holds the state for a descriptor version. This
// includes the lease information for a descriptor version.
// TODO(vivek): A node only needs to manage lease information on what it
// thinks is the latest version for a descriptor.
type descriptorVersionState struct {
	t *descriptorState
	// This descriptor is immutable and can be shared by many goroutines.
	// Care must be taken to not modify it.
	catalog.Descriptor

	mu struct {
		syncutil.Mutex

		// The expiration time for the descriptor version. A transaction with
		// timestamp T can use this descriptor version iff
		// Descriptor.GetDescriptorModificationTime() <= T < expiration
		//
		// The expiration time is either the expiration time of the lease when a lease
		// is associated with the version, or the ModificationTime of the next version
		// when the version isn't associated with a lease.
		expiration hlc.Timestamp

		// The session that was used to acquire this descriptor version, which is
		// only populated when the session based leasing mode is *at least* dual
		// write.
		session sqlliveness.Session

		refcount int
		// Set if the node has a lease on this descriptor version.
		// Leases can only be held for the two latest versions of
		// a descriptor. The latest version known to a node
		// (can be different than the current latest version in the store)
		// is always associated with a lease. The previous version known to
		// a node might not necessarily be associated with a lease.
		lease *storedLease
	}
}

func (s *descriptorVersionState) Release(ctx context.Context) {
	s.t.release(ctx, s)
}

func (s *descriptorVersionState) Underlying() catalog.Descriptor {
	return s.Descriptor
}

func (s *descriptorVersionState) Expiration() hlc.Timestamp {
	return s.getExpiration()
}

// SafeFormat implements redact.SafeFormatter.
func (s *descriptorVersionState) SafeFormat(w redact.SafePrinter, _ rune) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Print(s.stringLocked())
}

func (s *descriptorVersionState) String() string {
	return redact.StringWithoutMarkers(s)
}

// stringLocked reads mu.refcount and thus needs to have mu held.
func (s *descriptorVersionState) stringLocked() redact.RedactableString {
	var sessionID string
	if s.mu.session != nil {
		sessionID = s.mu.session.ID().String()
	}
	return redact.Sprintf("%d(%q,%s) ver=%d:%s, refcount=%d", s.GetID(), s.GetName(), redact.SafeString(sessionID), s.GetVersion(), s.mu.expiration, s.mu.refcount)
}

// hasExpired checks if the descriptor is too old to be used (by a txn
// operating) at the given timestamp.
func (s *descriptorVersionState) hasExpired(timestamp hlc.Timestamp) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hasExpiredLocked(timestamp)
}

// hasExpired checks if the descriptor is too old to be used (by a txn
// operating) at the given timestamp.
func (s *descriptorVersionState) hasExpiredLocked(timestamp hlc.Timestamp) bool {
	return s.getExpirationLocked().LessEq(timestamp)
}

func (s *descriptorVersionState) incRefCount(ctx context.Context, expensiveLogEnabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.incRefCountLocked(ctx, expensiveLogEnabled)
}

func (s *descriptorVersionState) incRefCountLocked(ctx context.Context, expensiveLogEnabled bool) {
	s.mu.refcount++
	if expensiveLogEnabled {
		log.VEventf(ctx, 2, "descriptorVersionState.incRefCount: %s", s.stringLocked())
	}
}

func (s *descriptorVersionState) getExpirationLocked() hlc.Timestamp {
	// A descriptor version state can now potentially contain two different types
	// of expiration:
	// 1) Fixed expirations, which will be based on some timestamp in the future,
	//   that will need to be renewed to keep a descriptor as "active"
	// 2) Session-based expirations, which say that a descriptor is in use,
	//    as long as the sqlliveness exists for it.
	// We are going to pick the longest possible leases between these two options,
	// assuming that session-based leases are being enforced. Session-based leases
	// will only be enforced once the Drain leasing mode is reached, which will stop
	// allowing fixed expiration leases from renewing (i.e. those leases will
	// eventually be *drained*.
	expiration := s.mu.expiration
	if s.mu.session != nil &&
		s.t.m.sessionBasedLeasingModeAtLeast(SessionBasedDrain) {
		sessionExpiry := s.mu.session.Expiration()
		if expiration.Less(sessionExpiry) {
			expiration = sessionExpiry
		}
	}
	return expiration
}

func (s *descriptorVersionState) getExpiration() hlc.Timestamp {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.getExpirationLocked()
}

// getStoredLease returns a copy of the stored lease.
func (s *descriptorVersionState) getStoredLease() *storedLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mu.lease == nil {
		return nil
	}
	leaseCopy := *s.mu.lease
	return &leaseCopy
}

// The lease expiration stored in the database is of a different type.
// We've decided that it's too much work to change the type to
// hlc.Timestamp, so we're using this method to give us the stored
// type: tree.DTimestamp.
func storedLeaseExpiration(expiration hlc.Timestamp) tree.DTimestamp {
	return tree.DTimestamp{Time: timeutil.Unix(0, expiration.WallTime).Round(time.Microsecond)}
}
