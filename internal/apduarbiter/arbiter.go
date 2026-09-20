package apduarbiter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iniwex5/vohive/pkg/logger"
)

var ErrAPDUBusy = errors.New("apdu busy")

type LeaseType string

const (
	LeaseTypeSession   LeaseType = "session"
	LeaseTypeOneShot   LeaseType = "oneshot"
	LeaseTypeTransport LeaseType = "transport"
	LeaseTypeBarrier   LeaseType = "barrier"
)

type TransportScope string

const (
	TransportScopeExclusive  TransportScope = "exclusive"
	TransportScopeQMIChannel TransportScope = "qmi_channel"
)

type APDUClass string

const (
	APDUClassEUICCWrite    APDUClass = "EUICCWrite"
	APDUClassEUICCRead     APDUClass = "EUICCRead"
	APDUClassUSIMAKA       APDUClass = "USIMAKA"
	APDUClassSMSC          APDUClass = "SMSC"
	APDUClassSwitchBarrier APDUClass = "SwitchBarrier"
	APDUClassRecovery      APDUClass = "Recovery"
)

const transportPriorityAging = 500 * time.Millisecond

type Options struct {
	// MaxLeaseHold 是单条 APDU transport lease 的无进展 watchdog 超时。
	// 持有方应在长耗时单条 APDU 前后调用 Touch；logical channel 生命周期不应再持有该 lease。
	MaxLeaseHold time.Duration
	// MaxSessions 是 legacy 兼容字段，仅服务旧 AcquireSession 接口。
	// 新生产路径应使用 AcquireTransport，默认单设备同一时刻只允许一个 active transport APDU。
	MaxSessions int
	// MaxQMITransports 限制 QMI logical-channel transport 并发数量。
	// 只有显式使用 TransportScopeQMIChannel 的 QMI channel APDU 会使用该并发窗口。
	MaxQMITransports int
}

type Request struct {
	Owner   string
	Mode    string
	Class   APDUClass
	Channel int
	Scope   TransportScope
}

type BarrierPolicy struct {
	BlockedClasses []APDUClass
}

type Stats struct {
	WaitRequests     uint64
	Acquires         uint64
	WaitTimeouts     uint64
	ForcedReleases   uint64
	CurrentQueue     int
	ActiveSessions   int
	ActiveOneshot    bool
	ActiveTransport  bool
	ActiveTransports int
	ActiveBarriers   int
	CurrentQueueType string
}

type SnapshotEntry struct {
	ID        uint64
	Owner     string
	Mode      string
	Class     APDUClass
	Channel   int
	Scope     TransportScope
	LeaseType LeaseType
	WaitMS    int64
	HoldMS    int64
}

type Snapshot struct {
	Active []SnapshotEntry
	Queue  []SnapshotEntry
}

type waitKind string

const (
	waitKindLease   waitKind = "lease"
	waitKindBarrier waitKind = "barrier"
)

type waitTicket struct {
	id        uint64
	owner     string
	mode      string
	class     APDUClass
	channel   int
	scope     TransportScope
	leaseType LeaseType
	kind      waitKind
	blocked   map[APDUClass]bool
	enqueued  time.Time
}

type activeLease struct {
	waitTicket
	acquiredAt time.Time
	expiresAt  time.Time
	timer      *time.Timer
}

type activeBarrier struct {
	waitTicket
	acquiredAt time.Time
}

type Arbiter struct {
	// 64 位 atomic 变量必须放在头部
	seq uint64

	deviceID string
	opts     Options

	mu               sync.Mutex
	queue            []waitTicket
	activeSessions   []*activeLease
	activeOneshot    *activeLease
	activeTransports []*activeLease
	activeBarriers   []*activeBarrier
	simAuthReady     simAuthReadyState
	notifyC          chan struct{}

	waitRequests   atomic.Uint64
	acquires       atomic.Uint64
	waitTimeouts   atomic.Uint64
	forcedReleases atomic.Uint64
}

type Lease struct {
	arbiter   *Arbiter
	id        uint64
	owner     string
	mode      string
	class     APDUClass
	channel   int
	scope     TransportScope
	leaseType LeaseType
	once      sync.Once
}

type Barrier struct {
	arbiter *Arbiter
	id      uint64
	owner   string
	mode    string
	class   APDUClass
	once    sync.Once
}

func New(deviceID string, opts Options) *Arbiter {
	if opts.MaxLeaseHold <= 0 {
		opts.MaxLeaseHold = 2 * time.Minute
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = 1
	}
	if opts.MaxQMITransports <= 0 {
		opts.MaxQMITransports = 1
	}
	return &Arbiter{
		deviceID: strings.TrimSpace(deviceID),
		opts:     opts,
		notifyC:  make(chan struct{}),
	}
}

func (a *Arbiter) AcquireTransport(ctx context.Context, req Request) (*Lease, error) {
	return a.acquire(ctx, normalizeRequest(req, LeaseTypeTransport), LeaseTypeTransport)
}

func (a *Arbiter) BeginBarrier(ctx context.Context, req Request, policy BarrierPolicy) (*Barrier, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req.Owner = normalizeOwner(req.Owner)
	req.Mode = normalizeMode(req.Mode)
	if req.Class == "" {
		req.Class = APDUClassSwitchBarrier
	}
	blocked := make(map[APDUClass]bool)
	for _, class := range policy.BlockedClasses {
		class = normalizeClass(class, LeaseTypeOneShot)
		if class != "" {
			blocked[class] = true
		}
	}
	if len(blocked) == 0 {
		blocked[APDUClassUSIMAKA] = true
		blocked[APDUClassSMSC] = true
	}

	ticket := waitTicket{
		id:        atomic.AddUint64(&a.seq, 1),
		owner:     req.Owner,
		mode:      req.Mode,
		class:     req.Class,
		channel:   req.Channel,
		scope:     TransportScopeExclusive,
		leaseType: LeaseTypeBarrier,
		kind:      waitKindBarrier,
		blocked:   blocked,
		enqueued:  time.Now(),
	}

	a.mu.Lock()
	a.queue = append(a.queue, ticket)
	a.waitRequests.Add(1)
	a.logEvent(ticket, "barrier_wait", len(a.queue), 0, 0, "")
	a.signalLocked()

	for {
		if a.canAcquireBarrierLocked(ticket) {
			a.removeTicketLocked(ticket.id)
			a.activeBarriers = append(a.activeBarriers, &activeBarrier{
				waitTicket: ticket,
				acquiredAt: time.Now(),
			})
			a.acquires.Add(1)
			waitMs := time.Since(ticket.enqueued).Milliseconds()
			a.logEvent(ticket, "barrier_enter", len(a.queue), waitMs, 0, "")
			a.signalLocked()
			a.mu.Unlock()
			return &Barrier{
				arbiter: a,
				id:      ticket.id,
				owner:   ticket.owner,
				mode:    ticket.mode,
				class:   ticket.class,
			}, nil
		}

		waitC := a.notifyC
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			a.mu.Lock()
			if a.removeTicketLocked(ticket.id) {
				a.waitTimeouts.Add(1)
				a.logEvent(ticket, "timeout", len(a.queue), time.Since(ticket.enqueued).Milliseconds(), 0, ctx.Err().Error())
				a.signalLocked()
			}
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: barrier owner=%s mode=%s class=%s: %v", ErrAPDUBusy, ticket.owner, ticket.mode, ticket.class, ctx.Err())
		case <-waitC:
			a.mu.Lock()
		}
	}
}

func (a *Arbiter) AcquireSession(ctx context.Context, owner, mode string) (*Lease, error) {
	return a.acquire(ctx, Request{Owner: owner, Mode: mode}, LeaseTypeSession)
}

func (a *Arbiter) AcquireOneShot(ctx context.Context, owner, mode string) (*Lease, error) {
	return a.acquire(ctx, Request{Owner: owner, Mode: mode}, LeaseTypeOneShot)
}

func (a *Arbiter) WaitIdle(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		a.mu.Lock()
		idle := a.isIdleLocked()
		waitC := a.notifyC
		a.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: wait idle: %v", ErrAPDUBusy, ctx.Err())
		case <-waitC:
		}
	}
}

func (a *Arbiter) IsIdle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isIdleLocked()
}

func (a *Arbiter) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	queueType := ""
	if len(a.queue) > 0 {
		queueType = string(a.queue[0].kind)
	}
	return Stats{
		WaitRequests:     a.waitRequests.Load(),
		Acquires:         a.acquires.Load(),
		WaitTimeouts:     a.waitTimeouts.Load(),
		ForcedReleases:   a.forcedReleases.Load(),
		CurrentQueue:     len(a.queue),
		ActiveSessions:   len(a.activeSessions),
		ActiveOneshot:    a.activeOneshot != nil,
		ActiveTransport:  len(a.activeTransports) > 0,
		ActiveTransports: len(a.activeTransports),
		ActiveBarriers:   len(a.activeBarriers),
		CurrentQueueType: queueType,
	}
}

func (a *Arbiter) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	active := make([]SnapshotEntry, 0, len(a.activeSessions)+len(a.activeTransports)+len(a.activeBarriers)+1)
	for _, session := range a.activeSessions {
		active = append(active, snapshotActiveLease(session, now))
	}
	if a.activeOneshot != nil {
		active = append(active, snapshotActiveLease(a.activeOneshot, now))
	}
	for _, transport := range a.activeTransports {
		active = append(active, snapshotActiveLease(transport, now))
	}
	for _, barrier := range a.activeBarriers {
		active = append(active, SnapshotEntry{
			ID:        barrier.id,
			Owner:     barrier.owner,
			Mode:      barrier.mode,
			Class:     barrier.class,
			Channel:   barrier.channel,
			Scope:     barrier.scope,
			LeaseType: LeaseTypeBarrier,
			HoldMS:    now.Sub(barrier.acquiredAt).Milliseconds(),
		})
	}
	queue := make([]SnapshotEntry, 0, len(a.queue))
	for _, ticket := range a.queue {
		queue = append(queue, SnapshotEntry{
			ID:        ticket.id,
			Owner:     ticket.owner,
			Mode:      ticket.mode,
			Class:     ticket.class,
			Channel:   ticket.channel,
			Scope:     ticket.scope,
			LeaseType: ticket.leaseType,
			WaitMS:    now.Sub(ticket.enqueued).Milliseconds(),
		})
	}
	return Snapshot{Active: active, Queue: queue}
}

func (l *Lease) Touch() bool {
	if l == nil || l.arbiter == nil {
		return false
	}
	return l.arbiter.touch(l.id, l.leaseType)
}

func (l *Lease) Active() bool {
	if l == nil || l.arbiter == nil {
		return false
	}
	return l.arbiter.isActive(l.id, l.leaseType)
}

func (l *Lease) Release() {
	if l == nil || l.arbiter == nil {
		return
	}
	l.once.Do(func() {
		l.arbiter.release(l.id, l.leaseType, "release")
	})
}

func (b *Barrier) Release() {
	if b == nil || b.arbiter == nil {
		return
	}
	b.once.Do(func() {
		b.arbiter.releaseBarrier(b.id)
	})
}

func (a *Arbiter) acquire(ctx context.Context, req Request, leaseType LeaseType) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req = normalizeRequest(req, leaseType)
	ticket := waitTicket{
		id:        atomic.AddUint64(&a.seq, 1),
		owner:     req.Owner,
		mode:      req.Mode,
		class:     req.Class,
		channel:   req.Channel,
		scope:     req.Scope,
		leaseType: leaseType,
		kind:      waitKindLease,
		enqueued:  time.Now(),
	}

	a.mu.Lock()
	a.queue = append(a.queue, ticket)
	a.waitRequests.Add(1)
	a.logEvent(ticket, "wait", len(a.queue), 0, 0, "")
	a.signalLocked()

	for {
		if a.canAcquireLeaseLocked(ticket) {
			a.removeTicketLocked(ticket.id)
			active := &activeLease{
				waitTicket: ticket,
				acquiredAt: time.Now(),
			}
			if a.opts.MaxLeaseHold > 0 {
				active.expiresAt = active.acquiredAt.Add(a.opts.MaxLeaseHold)
				leaseID := ticket.id
				lType := leaseType
				active.timer = time.AfterFunc(a.opts.MaxLeaseHold, func() {
					a.releaseExpired(leaseID, lType)
				})
			}
			a.addActiveLocked(active)
			a.acquires.Add(1)
			waitMs := time.Since(ticket.enqueued).Milliseconds()
			a.logEvent(ticket, "acquire", len(a.queue), waitMs, 0, "")
			a.signalLocked()
			a.mu.Unlock()
			return &Lease{
				arbiter:   a,
				id:        ticket.id,
				owner:     ticket.owner,
				mode:      ticket.mode,
				class:     ticket.class,
				channel:   ticket.channel,
				scope:     ticket.scope,
				leaseType: leaseType,
			}, nil
		}

		waitC := a.notifyC
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			a.mu.Lock()
			if a.removeTicketLocked(ticket.id) {
				a.waitTimeouts.Add(1)
				a.logEvent(ticket, "timeout", len(a.queue), time.Since(ticket.enqueued).Milliseconds(), 0, ctx.Err().Error())
				a.signalLocked()
			}
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: owner=%s mode=%s class=%s channel=%d: %v", ErrAPDUBusy, ticket.owner, ticket.mode, ticket.class, ticket.channel, ctx.Err())
		case <-waitC:
			a.mu.Lock()
		}
	}
}

func (a *Arbiter) canAcquireLeaseLocked(ticket waitTicket) bool {
	if ticket.kind != waitKindLease {
		return false
	}
	if a.isBlockedByActiveBarrierLocked(ticket.class) {
		return false
	}
	if a.hasPendingBarrierAheadLocked(ticket) {
		return false
	}

	switch ticket.leaseType {
	case LeaseTypeSession:
		return a.queueHeadIsLocked(ticket.id) && a.activeOneshot == nil && !a.hasActiveTransportLocked() && len(a.activeSessions) < a.opts.MaxSessions
	case LeaseTypeOneShot:
		return a.queueHeadIsLocked(ticket.id) && len(a.activeSessions) == 0 && a.activeOneshot == nil && !a.hasActiveTransportLocked()
	case LeaseTypeTransport:
		if len(a.activeSessions) > 0 || a.activeOneshot != nil {
			return false
		}
		return a.selectedTransportLocked() == ticket.id
	default:
		return false
	}
}

func (a *Arbiter) canAcquireBarrierLocked(ticket waitTicket) bool {
	if ticket.kind != waitKindBarrier {
		return false
	}
	if a.hasActiveTransportLocked() || len(a.activeSessions) > 0 || a.activeOneshot != nil {
		return false
	}
	return a.selectedBarrierLocked() == ticket.id
}

func (a *Arbiter) selectedBarrierLocked() uint64 {
	for _, ticket := range a.queue {
		if ticket.kind == waitKindBarrier {
			return ticket.id
		}
	}
	return 0
}

func (a *Arbiter) selectedTransportLocked() uint64 {
	var oldestEligible *waitTicket
	var oldestAgedNonAKA *waitTicket
	var oldestAKA *waitTicket
	now := time.Now()
	for i := range a.queue {
		ticket := &a.queue[i]
		if ticket.kind == waitKindBarrier {
			break
		}
		if ticket.leaseType != LeaseTypeTransport {
			break
		}
		if a.isBlockedByActiveBarrierLocked(ticket.class) {
			continue
		}
		if !a.canAcquireTransportNowLocked(*ticket) {
			if a.transportWaitGatesQueueLocked(*ticket) {
				break
			}
			continue
		}
		if oldestEligible == nil {
			oldestEligible = ticket
		}
		if ticket.class == APDUClassUSIMAKA {
			if oldestAKA == nil {
				oldestAKA = ticket
			}
			continue
		}
		if oldestAgedNonAKA == nil && now.Sub(ticket.enqueued) >= transportPriorityAging {
			oldestAgedNonAKA = ticket
		}
	}
	switch {
	case oldestAgedNonAKA != nil:
		return oldestAgedNonAKA.id
	case oldestAKA != nil:
		return oldestAKA.id
	case oldestEligible != nil:
		return oldestEligible.id
	default:
		return 0
	}
}

func (a *Arbiter) canAcquireTransportNowLocked(ticket waitTicket) bool {
	if ticket.leaseType != LeaseTypeTransport {
		return false
	}
	if ticket.scope != TransportScopeQMIChannel {
		return !a.hasActiveTransportLocked()
	}
	if a.hasActiveExclusiveTransportLocked() {
		return false
	}
	if a.activeQMITransportCountLocked() >= a.opts.MaxQMITransports {
		return false
	}
	return !a.hasActiveTransportOnChannelLocked(ticket.channel)
}

func (a *Arbiter) transportWaitGatesQueueLocked(ticket waitTicket) bool {
	if ticket.leaseType != LeaseTypeTransport {
		return true
	}
	if ticket.scope != TransportScopeQMIChannel {
		return true
	}
	if a.hasActiveExclusiveTransportLocked() || a.activeQMITransportCountLocked() >= a.opts.MaxQMITransports {
		return true
	}
	return false
}

func (a *Arbiter) hasPendingBarrierAheadLocked(ticket waitTicket) bool {
	for _, queued := range a.queue {
		if queued.id == ticket.id {
			return false
		}
		if queued.kind != waitKindBarrier {
			continue
		}
		if queued.blocked[ticket.class] {
			return true
		}
	}
	return false
}

func (a *Arbiter) isBlockedByActiveBarrierLocked(class APDUClass) bool {
	for _, barrier := range a.activeBarriers {
		if barrier.blocked[class] {
			return true
		}
	}
	return false
}

func (a *Arbiter) queueHeadIsLocked(ticketID uint64) bool {
	return len(a.queue) > 0 && a.queue[0].id == ticketID
}

func (a *Arbiter) addActiveLocked(active *activeLease) {
	switch active.leaseType {
	case LeaseTypeSession:
		a.activeSessions = append(a.activeSessions, active)
	case LeaseTypeOneShot:
		a.activeOneshot = active
	case LeaseTypeTransport:
		a.activeTransports = append(a.activeTransports, active)
	}
}

func (a *Arbiter) touch(leaseID uint64, leaseType LeaseType) bool {
	if a.opts.MaxLeaseHold <= 0 {
		return a.isActive(leaseID, leaseType)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	active := a.findActiveLocked(leaseID, leaseType)
	if active == nil {
		return false
	}
	active.expiresAt = time.Now().Add(a.opts.MaxLeaseHold)
	if active.timer == nil {
		lType := leaseType
		active.timer = time.AfterFunc(a.opts.MaxLeaseHold, func() {
			a.releaseExpired(leaseID, lType)
		})
		return true
	}
	active.timer.Reset(a.opts.MaxLeaseHold)
	return true
}

func (a *Arbiter) release(leaseID uint64, leaseType LeaseType, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	released := a.removeActiveLocked(leaseID, leaseType)
	if released == nil {
		return
	}
	if released.timer != nil {
		released.timer.Stop()
	}
	holdMs := time.Since(released.acquiredAt).Milliseconds()
	phase := "release"
	if reason == "force-timeout" {
		phase = "force-release"
		a.forcedReleases.Add(1)
	}
	a.logEvent(released.waitTicket, phase, len(a.queue), 0, holdMs, reason)
	a.signalLocked()
}

func (a *Arbiter) releaseBarrier(barrierID uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for i, barrier := range a.activeBarriers {
		if barrier.id != barrierID {
			continue
		}
		a.activeBarriers = append(a.activeBarriers[:i], a.activeBarriers[i+1:]...)
		holdMs := time.Since(barrier.acquiredAt).Milliseconds()
		a.logEvent(barrier.waitTicket, "barrier_leave", len(a.queue), 0, holdMs, "")
		a.signalLocked()
		return
	}
}

func (a *Arbiter) releaseExpired(leaseID uint64, leaseType LeaseType) {
	a.mu.Lock()
	defer a.mu.Unlock()

	active := a.findActiveLocked(leaseID, leaseType)
	if active == nil {
		return
	}
	if !active.expiresAt.IsZero() {
		if remaining := time.Until(active.expiresAt); remaining > 0 {
			if active.timer != nil {
				active.timer.Reset(remaining)
			}
			return
		}
	}

	released := a.removeActiveLocked(leaseID, leaseType)
	if released == nil {
		return
	}
	if released.timer != nil {
		released.timer.Stop()
	}
	a.forcedReleases.Add(1)
	holdMs := time.Since(released.acquiredAt).Milliseconds()
	a.logEvent(released.waitTicket, "force-release", len(a.queue), 0, holdMs, "force-timeout")
	a.signalLocked()
}

func (a *Arbiter) isActive(leaseID uint64, leaseType LeaseType) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.findActiveLocked(leaseID, leaseType) != nil
}

func (a *Arbiter) findActiveLocked(leaseID uint64, leaseType LeaseType) *activeLease {
	switch leaseType {
	case LeaseTypeSession:
		for _, s := range a.activeSessions {
			if s.id == leaseID {
				return s
			}
		}
	case LeaseTypeOneShot:
		if a.activeOneshot != nil && a.activeOneshot.id == leaseID {
			return a.activeOneshot
		}
	case LeaseTypeTransport:
		for _, t := range a.activeTransports {
			if t.id == leaseID {
				return t
			}
		}
	}
	return nil
}

func (a *Arbiter) removeActiveLocked(leaseID uint64, leaseType LeaseType) *activeLease {
	switch leaseType {
	case LeaseTypeSession:
		for i, s := range a.activeSessions {
			if s.id == leaseID {
				a.activeSessions = append(a.activeSessions[:i], a.activeSessions[i+1:]...)
				return s
			}
		}
	case LeaseTypeOneShot:
		if a.activeOneshot != nil && a.activeOneshot.id == leaseID {
			released := a.activeOneshot
			a.activeOneshot = nil
			return released
		}
	case LeaseTypeTransport:
		for i, t := range a.activeTransports {
			if t.id == leaseID {
				a.activeTransports = append(a.activeTransports[:i], a.activeTransports[i+1:]...)
				return t
			}
		}
	}
	return nil
}

func (a *Arbiter) removeTicketLocked(ticketID uint64) bool {
	for i := range a.queue {
		if a.queue[i].id != ticketID {
			continue
		}
		a.queue = append(a.queue[:i], a.queue[i+1:]...)
		return true
	}
	return false
}

func (a *Arbiter) isIdleLocked() bool {
	return a.activeOneshot == nil &&
		len(a.activeTransports) == 0 &&
		len(a.activeSessions) == 0 &&
		len(a.activeBarriers) == 0 &&
		len(a.queue) == 0
}

func (a *Arbiter) hasActiveTransportLocked() bool {
	return len(a.activeTransports) > 0
}

func (a *Arbiter) hasActiveExclusiveTransportLocked() bool {
	for _, transport := range a.activeTransports {
		if transport.scope != TransportScopeQMIChannel {
			return true
		}
	}
	return false
}

func (a *Arbiter) activeQMITransportCountLocked() int {
	count := 0
	for _, transport := range a.activeTransports {
		if transport.scope == TransportScopeQMIChannel {
			count++
		}
	}
	return count
}

func (a *Arbiter) hasActiveTransportOnChannelLocked(channel int) bool {
	for _, transport := range a.activeTransports {
		if transport.scope == TransportScopeQMIChannel && transport.channel == channel {
			return true
		}
	}
	return false
}

func (a *Arbiter) signalLocked() {
	close(a.notifyC)
	a.notifyC = make(chan struct{})
}

func shouldLogEvent(phase string, queueLen int, waitMs int64, holdMs int64) bool {
	if phase == "timeout" || phase == "force-release" || strings.HasPrefix(phase, "barrier_") {
		return true
	}
	if waitMs > 0 || holdMs >= 500 || queueLen > 1 {
		return true
	}
	return false
}

func (a *Arbiter) logEvent(ticket waitTicket, phase string, queueLen int, waitMs int64, holdMs int64, reason string) {
	if !shouldLogEvent(phase, queueLen, waitMs, holdMs) {
		return
	}

	fields := []any{
		"device", a.deviceID,
		"owner", ticket.owner,
		"mode", ticket.mode,
		"class", string(ticket.class),
		"channel", ticket.channel,
		"scope", string(ticket.scope),
		"lease_type", string(ticket.leaseType),
		"phase", phase,
		"queue_len", queueLen,
		"wait_ms", waitMs,
		"hold_ms", holdMs,
	}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	switch phase {
	case "timeout", "force-release":
		logger.Warn("APDU 仲裁事件", fields...)
	default:
		logger.Debug("APDU 仲裁事件", fields...)
	}
}

func snapshotActiveLease(active *activeLease, now time.Time) SnapshotEntry {
	return SnapshotEntry{
		ID:        active.id,
		Owner:     active.owner,
		Mode:      active.mode,
		Class:     active.class,
		Channel:   active.channel,
		Scope:     active.scope,
		LeaseType: active.leaseType,
		HoldMS:    now.Sub(active.acquiredAt).Milliseconds(),
	}
}

func normalizeRequest(req Request, leaseType LeaseType) Request {
	req.Owner = normalizeOwner(req.Owner)
	req.Mode = normalizeMode(req.Mode)
	req.Class = normalizeClass(req.Class, leaseType)
	req.Scope = normalizeTransportScope(req.Scope, req.Mode, req.Channel, leaseType)
	return req
}

func normalizeTransportScope(scope TransportScope, mode string, channel int, leaseType LeaseType) TransportScope {
	if leaseType != LeaseTypeTransport {
		return TransportScopeExclusive
	}
	if scope == TransportScopeQMIChannel && mode == "QMI" && channel > 0 {
		return TransportScopeQMIChannel
	}
	return TransportScopeExclusive
}

func normalizeClass(class APDUClass, leaseType LeaseType) APDUClass {
	if class != "" {
		return class
	}
	switch leaseType {
	case LeaseTypeTransport:
		return APDUClassEUICCWrite
	case LeaseTypeOneShot:
		return APDUClassUSIMAKA
	default:
		return APDUClassEUICCWrite
	}
}

func normalizeOwner(owner string) string {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return "unknown"
	}
	return owner
}

func normalizeMode(mode string) string {
	mode = strings.ToUpper(strings.TrimSpace(mode))
	if mode == "" {
		return "UNKNOWN"
	}
	return mode
}
