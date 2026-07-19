package team

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Clock supplies deterministic Team lifecycle timestamps.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (function ClockFunc) Now() time.Time { return function() }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// EventIDSource creates safe unique transition IDs.
type EventIDSource func(time.Time) (EventID, error)

// Option configures an Engine.
type Option func(*engineConfig) error

type engineConfig struct {
	clock Clock
	ids   EventIDSource
}

// WithClock replaces the Engine clock.
func WithClock(clock Clock) Option {
	return func(config *engineConfig) error {
		if clock == nil {
			return fmt.Errorf("%w: nil clock", ErrInvalid)
		}

		config.clock = clock

		return nil
	}
}

// WithEventIDSource replaces transition ID generation.
func WithEventIDSource(source EventIDSource) Option {
	return func(config *engineConfig) error {
		if source == nil {
			return fmt.Errorf("%w: nil event ID source", ErrInvalid)
		}

		config.ids = source

		return nil
	}
}

// Engine applies finite commands to durable Team aggregates.
type Engine struct {
	store Store
	clock Clock
	ids   EventIDSource
}

// New constructs an Engine over a Team Store. It starts no background work.
func New(store Store, options ...Option) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalid)
	}

	config := engineConfig{clock: systemClock{}, ids: randomEventID}

	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil engine option", ErrInvalid)
		}

		if err := option(&config); err != nil {
			return nil, err
		}
	}

	return &Engine{store: store, clock: config.clock, ids: config.ids}, nil
}

// Create persists a new active Team with exactly one lead member.
//
//nolint:gocyclo,nestif // Creation includes validation, replay, construction, and lost-response recovery.
func (engine *Engine) Create(ctx context.Context, request CreateRequest) (Team, error) {
	if err := validateCommand(request.Command, true); err != nil {
		return Team{}, err
	}

	if request.Command.Actor.Kind != ActorKindHostRuntime {
		return Team{}, ErrUnauthorized
	}

	if err := validateSafeID("Team id", string(request.ID)); err != nil {
		return Team{}, err
	}

	if err := validateText("objective", request.Objective, maxObjectiveBytes, true); err != nil {
		return Team{}, err
	}

	if err := validateMemberSpec(request.Lead); err != nil {
		return Team{}, err
	}

	limits, err := resolveLimits(request.Limits)
	if err != nil {
		return Team{}, err
	}

	hash, err := commandHash("create_team", request.ID, request.Command.Actor, struct {
		Objective string     `json:"objective"`
		Lead      MemberSpec `json:"lead"`
		Limits    Limits     `json:"limits"`
	}{request.Objective, request.Lead, limits})
	if err != nil {
		return Team{}, err
	}

	if replay, found, replayErr := engine.replay(ctx, request.ID, request.Command.ID, hash); found || replayErr != nil {
		if replayErr != nil {
			return Team{}, replayErr
		}

		return cloneTeam(replay.Team), nil
	}

	now := utc(engine.clock.Now())

	eventID, err := engine.newEventID(now)
	if err != nil {
		return Team{}, err
	}

	team := Team{
		SchemaVersion: schemaVersion,
		ID:            request.ID,
		Revision:      1,
		Status:        StatusActive,
		Objective:     request.Objective,
		LeadMemberID:  request.Lead.ID,
		Members: []Member{{
			ID: request.Lead.ID, Name: request.Lead.Name, Role: request.Lead.Role,
			SessionRef:           request.Lead.SessionRef,
			CapabilityProfileRef: request.Lead.CapabilityProfileRef,
			Status:               MemberStatusActive, RegisteredAt: now,
		}},
		Tasks: make([]Task, 0), Messages: make([]Message, 0),
		NextMessageSequence: 1, Limits: limits, CreatedAt: now, UpdatedAt: now,
	}

	record := Record{
		SchemaVersion: schemaVersion,
		Team:          team,
		Transition: Transition{
			SchemaVersion: schemaVersion, ID: eventID, TeamID: team.ID, Revision: 1,
			At: now, Actor: request.Command.Actor, CommandID: request.Command.ID,
			CommandHash: hash, Cause: CauseCreate, To: StatusActive,
			MemberID: request.Lead.ID,
		},
	}
	if err := engine.store.Create(ctx, record); err != nil {
		if errors.Is(err, ErrExists) {
			if replay, found, replayErr := engine.replay(ctx, request.ID, request.Command.ID, hash); found || replayErr != nil {
				if replayErr != nil {
					return Team{}, replayErr
				}

				return cloneTeam(replay.Team), nil
			}
		}

		return Team{}, err
	}

	return cloneTeam(team), nil
}

// Get returns the latest Team snapshot.
func (engine *Engine) Get(ctx context.Context, id ID) (Team, error) {
	if err := validateSafeID("Team id", string(id)); err != nil {
		return Team{}, err
	}

	record, err := engine.loadRecord(ctx, id)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// List returns a bounded page of current Team snapshots.
func (engine *Engine) List(ctx context.Context, options ListOptions) (ListPage, error) {
	if err := validateListOptions(options, defaultMaxListPage); err != nil {
		return ListPage{}, err
	}

	page, err := engine.store.List(ctx, options)
	if err != nil {
		return ListPage{}, err
	}

	if err := validateListPage(page, options); err != nil {
		return ListPage{}, err
	}

	return cloneListPage(page), nil
}

// History returns cloned durable records in revision order.
func (engine *Engine) History(ctx context.Context, id ID) ([]Record, error) {
	if err := validateSafeID("Team id", string(id)); err != nil {
		return nil, err
	}

	records, err := engine.store.History(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := validateLoadedHistory(id, records); err != nil {
		return nil, err
	}

	return cloneRecords(records), nil
}

type transitionFields struct {
	cause     Cause
	taskID    TaskID
	memberID  MemberID
	attemptID AttemptID
	messageID MessageID
	reason    string
}

type mutation func(*Team, time.Time) (transitionFields, error)

//nolint:gocyclo // Shared command application keeps replay and CAS race handling in one transaction path.
func (engine *Engine) apply(
	ctx context.Context,
	id ID,
	command CommandMetadata,
	operation string,
	semantic any,
	mutate mutation,
) (Record, error) {
	if err := validateSafeID("Team id", string(id)); err != nil {
		return Record{}, err
	}

	if err := validateCommand(command, false); err != nil {
		return Record{}, err
	}

	hash, err := commandHash(operation, id, command.Actor, semantic)
	if err != nil {
		return Record{}, err
	}

	if replay, found, replayErr := engine.replay(ctx, id, command.ID, hash); found || replayErr != nil {
		return replay, replayErr
	}

	current, err := engine.loadRecord(ctx, id)
	if err != nil {
		return Record{}, err
	}

	if current.Team.Revision != command.ExpectedRevision {
		return Record{}, &ConflictError{
			Expected: command.ExpectedRevision,
			Actual:   current.Team.Revision,
		}
	}

	now := utc(engine.clock.Now())
	nextTeam := cloneTeam(current.Team)

	fields, err := mutate(&nextTeam, now)
	if err != nil {
		return Record{}, err
	}

	eventID, err := engine.newEventID(now)
	if err != nil {
		return Record{}, err
	}

	nextTeam.Revision = current.Team.Revision + 1
	nextTeam.UpdatedAt = now
	next := Record{
		SchemaVersion: schemaVersion,
		Team:          nextTeam,
		Transition: Transition{
			SchemaVersion: schemaVersion, ID: eventID, TeamID: id,
			Revision: nextTeam.Revision, At: now, Actor: command.Actor,
			CommandID: command.ID, CommandHash: hash, Cause: fields.cause,
			TaskID: fields.taskID, MemberID: fields.memberID,
			AttemptID: fields.attemptID, MessageID: fields.messageID,
			From: current.Team.Status, To: nextTeam.Status, Reason: fields.reason,
		},
	}

	if err := engine.store.CompareAndSwap(ctx, id, command.ExpectedRevision, next); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrCommandConflict) {
			if replay, found, replayErr := engine.replay(ctx, id, command.ID, hash); found || replayErr != nil {
				return replay, replayErr
			}
		}

		return Record{}, err
	}

	return cloneRecord(next), nil
}

func (engine *Engine) replay(
	ctx context.Context,
	id ID,
	commandID CommandID,
	hash string,
) (Record, bool, error) {
	record, err := engine.store.LoadCommand(ctx, id, commandID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Record{}, false, nil
		}

		return Record{}, false, err
	}

	if err := validateLoadedRecord(id, record); err != nil {
		return Record{}, false, err
	}

	if record.Transition.CommandID != commandID {
		return Record{}, false, corruptStoreValue(id, "command lookup returned another command", nil)
	}

	if record.Transition.CommandHash != hash {
		return Record{}, true, ErrCommandConflict
	}

	return cloneRecord(record), true, nil
}

func (engine *Engine) loadRecord(ctx context.Context, id ID) (Record, error) {
	record, err := engine.store.Load(ctx, id)
	if err != nil {
		return Record{}, err
	}

	if err := validateLoadedRecord(id, record); err != nil {
		return Record{}, err
	}

	return record, nil
}

func (engine *Engine) newEventID(now time.Time) (EventID, error) {
	id, err := engine.ids(now)
	if err != nil {
		return "", fmt.Errorf("team: generate event ID: %w", err)
	}

	if err := validateSafeID("event id", string(id)); err != nil {
		return "", err
	}

	return id, nil
}

func commandHash(operation string, id ID, actor Actor, semantic any) (string, error) {
	payload, err := json.Marshal(struct {
		Operation string `json:"operation"`
		TeamID    ID     `json:"team_id"`
		Actor     Actor  `json:"actor"`
		Semantic  any    `json:"semantic"`
	}{operation, id, actor, semantic})
	if err != nil {
		return "", fmt.Errorf("team: encode command: %w", err)
	}

	digest := sha256.Sum256(payload)

	return hex.EncodeToString(digest[:]), nil
}

func randomEventID(at time.Time) (EventID, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}

	return EventID(fmt.Sprintf(
		"event-%s-%s",
		at.UTC().Format("20060102T150405.000000000Z"),
		hex.EncodeToString(suffix[:]),
	)), nil
}
