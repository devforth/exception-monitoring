package sqlite

// The storage-neutral types and values of package store, under the names
// this package's SQL code uses.

import domain "github.com/devforth/exception-monitoring/internal/storage/store"

// Page is domain.Page (generic: call it there)
const DateAnyEvent = domain.DateAnyEvent
const DateFirstSeen = domain.DateFirstSeen
const DateLastSeen = domain.DateLastSeen
const GrantOAuth = domain.GrantOAuth
const GrantToken = domain.GrantToken
const MaxIssueBatch = domain.MaxIssueBatch
const MaxVerifyAttempts = domain.MaxVerifyAttempts
const OutboxFailed = domain.OutboxFailed
const OutboxPending = domain.OutboxPending
const OutboxSent = domain.OutboxSent
const RoleAdmin = domain.RoleAdmin
const RoleSuperadmin = domain.RoleSuperadmin
const RoleUser = domain.RoleUser
const SourceAPI = domain.SourceAPI
const SourceEvent = domain.SourceEvent
const SourceMCP = domain.SourceMCP
const SourceUI = domain.SourceUI
const StatusIgnored = domain.StatusIgnored
const StatusResolved = domain.StatusResolved
const StatusUnresolved = domain.StatusUnresolved
const VerifyExpired = domain.VerifyExpired
const VerifyOK = domain.VerifyOK
const VerifyTooManyTries = domain.VerifyTooManyTries
const VerifyWrong = domain.VerifyWrong
const WritePause = domain.WritePause

type Actor = domain.Actor
type Connection = domain.Connection
type DeliveryHealth = domain.DeliveryHealth
type Destination = domain.Destination
type Event = domain.Event
type EventFilter = domain.EventFilter
type EventSummary = domain.EventSummary
type Grant = domain.Grant
type GrantTokens = domain.GrantTokens
type Issue = domain.Issue
type IssueFilter = domain.IssueFilter
type LogFilter = domain.LogFilter
type LogIssue = domain.LogIssue
type LogRecord = domain.LogRecord
type NewEvent = domain.NewEvent
type NewLog = domain.NewLog
type OAuthClient = domain.OAuthClient
type OAuthCode = domain.OAuthCode
type OutboxFilter = domain.OutboxFilter
type OutboxMessage = domain.OutboxMessage
type OutboxRecord = domain.OutboxRecord
type PasswordChange = domain.PasswordChange
type Project = domain.Project
type ProjectScope = domain.ProjectScope
type SaveResult = domain.SaveResult
type StatusChange = domain.StatusChange
type User = domain.User
type VerifyResult = domain.VerifyResult

var ErrNotFound = domain.ErrNotFound
var ErrNotVerified = domain.ErrNotVerified
var ErrOAuthCapacity = domain.ErrOAuthCapacity
var ErrProjectConflict = domain.ErrProjectConflict
var ErrUsernameTaken = domain.ErrUsernameTaken
var NewConnection = domain.NewConnection
var NormalizeIssueIDs = domain.NormalizeIssueIDs
var Pause = domain.Pause
var RoleSummaries = domain.RoleSummaries
var ValidDateField = domain.ValidDateField
var ValidIssueSort = domain.ValidIssueSort

// Store implements the storage of package store.
var _ domain.Store = (*Store)(nil)
