package service

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

const latestWalSchemaID uint16 = 6

// sync intervals
const (
	pullIntervalSeconds       = 5
	pushWaitDuration          = time.Second * time.Duration(5)
	gatherFileNumberThreshold = 10
)

var EmailRegex = regexp.MustCompile("[a-zA-Z0-9.!#$%&'*+\\/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*")

func generateUUID() uuid {
	return uuid(rand.Uint32())
}

type walItemSchema2 struct {
	UUID                       uuid
	TargetUUID                 uuid
	ListItemCreationTime       int64
	TargetListItemCreationTime int64
	EventTime                  int64
	EventType                  EventType
	LineLength                 uint64
	NoteExists                 bool
	NoteLength                 uint64
}

var (
	errWalIntregrity = errors.New("the wal was forcefully recovered, r.log needs to be purged")
)

type EventType uint16

// Ordering of these enums are VERY IMPORTANT as they're used for comparisons when resolving WAL merge conflicts
// (although there has to be nanosecond level collisions in order for this to be relevant)
const (
	NullEvent EventType = iota
	AddEvent
	UpdateEvent
	MoveUpEvent
	MoveDownEvent
	ShowEvent
	HideEvent
	DeleteEvent
)

type LineFriendsSchema4 struct {
	IsProcessed bool
	Offset      int
	Emails      map[string]struct{}
}

type EventLogSchema4 struct {
	UUID, TargetUUID           uuid
	ListItemCreationTime       int64
	TargetListItemCreationTime int64
	UnixNanoTime               int64
	EventType                  EventType
	Line                       string
	Note                       []byte
	Friends                    LineFriendsSchema4
	key, targetKey             string
}

type LineFriends struct {
	IsProcessed bool
	Offset      int
	Emails      []string
	emailsMap   map[string]struct{}
}

type EventLogSchema5 struct {
	UUID, TargetUUID           uuid
	ListItemCreationTime       int64
	TargetListItemCreationTime int64
	UnixNanoTime               int64
	EventType                  EventType
	Line                       string
	Note                       []byte
	Friends                    LineFriends
	key, targetKey             string
}

type EventLog struct {
	UUID                           uuid
	LamportTimestamp               int64
	EventType                      EventType
	ListItemKey, TargetListItemKey string
	Line                           string
	Note                           []byte
	Friends                        LineFriends
	cachedKey                      string
}

func (e *EventLog) key() string {
	if e.cachedKey == "" {
		e.cachedKey = strconv.Itoa(int(e.UUID)) + ":" + strconv.Itoa(int(e.LamportTimestamp))
	}
	return e.cachedKey
}

func (e *EventLog) emailHasAccess(email string) bool {
	if e.Friends.emailsMap == nil {
		e.Friends.emailsMap = make(map[string]struct{})
		for _, f := range e.Friends.Emails {
			e.Friends.emailsMap[f] = struct{}{}
		}
	}
	_, exists := e.Friends.emailsMap[email]
	return exists
}

// WalFile offers a generic interface into local or remote filesystems
type WalFile interface {
	GetUUID() string
	GetRoot() string
	GetMatchingWals(context.Context, string) ([]string, error)
	GetWalBytes(context.Context, io.Writer, string) error
	RemoveWals(context.Context, []string) error
	Flush(context.Context, *bytes.Buffer, string) error
}

type LocalWalFile interface {
	Purge()

	WalFile
}

type LocalFileWalFile struct {
	rootDir string
}

func NewLocalFileWalFile(rootDir string) *LocalFileWalFile {
	return &LocalFileWalFile{
		rootDir: rootDir,
	}
}

func (wf *LocalFileWalFile) Purge() {
	if err := os.RemoveAll(wf.rootDir); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}

func (wf *LocalFileWalFile) GetUUID() string {
	return "local"
}

func (wf *LocalFileWalFile) GetRoot() string {
	return wf.rootDir
}

func (wf *LocalFileWalFile) GetMatchingWals(ctx context.Context, matchPattern string) ([]string, error) {
	pullPaths, err := filepath.Glob(matchPattern)
	if err != nil {
		return []string{}, err
	}
	uuids := []string{}
	for _, p := range pullPaths {
		_, fileName := path.Split(p)
		uuid := strings.Split(strings.Split(fileName, "_")[1], ".")[0]
		uuids = append(uuids, uuid)
	}
	return uuids, nil
}

func (wf *LocalFileWalFile) GetWalBytes(ctx context.Context, w io.Writer, fileName string) error {
	//var b []byte
	fileName = fmt.Sprintf(path.Join(wf.GetRoot(), walFilePattern), fileName)
	f, err := os.Open(fileName)
	if err != nil {
		return nil
	}
	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	//b, err := ioutil.ReadFile(fileName)
	//if err != nil {
	//    return b, err
	//}
	return nil
}

func (wf *LocalFileWalFile) RemoveWals(ctx context.Context, fileNames []string) error {
	for _, f := range fileNames {
		os.Remove(fmt.Sprintf(path.Join(wf.GetRoot(), walFilePattern), f))
	}
	return nil
}

func (wf *LocalFileWalFile) Flush(ctx context.Context, b *bytes.Buffer, randomUUID string) error {
	fileName := fmt.Sprintf(path.Join(wf.GetRoot(), walFilePattern), randomUUID)
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(b.Bytes()); err != nil {
		return err
	}
	return nil
}

// https://go.dev/play/p/1kbFF8FR-V7
// enforces existence of surrounding boundary character
// match[0] = full match (inc boundaries)
// match[1] = `@email`
// match[2] = `email`
//var lineFriendRegex = regexp.MustCompile("(?:^|\\s)(@([a-zA-Z0-9.!#$%&'*+\\/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*))(?:$|\\s)")

// getFriendsFromLine returns a map of @friends in the line, that currently exist in the friends cache, and a
// boolean representing whether or not the line is already correctly ordered (e.g. the friends are all appended
// to the end of the line, separated by single whitespace chars)
func (r *DBListRepo) getFriendsFromLine(line string, existingFriends []string) ([]string, bool) {
	// (golang?) regex does return overlapping results, which we need in order to ensure space
	// or start/end email boundaries. Therefore we iterate over the line and match/replace any
	// email with (pre|app)ending spaces with a single space

	//friends := map[string]struct{}{}

	//if len(line) == 0 {
	//    return friends
	//}

	//replacedLine := line
	//oldReplacedLine := ""
	//for replacedLine != oldReplacedLine {
	//    oldReplacedLine = replacedLine
	//    if match := lineFriendRegex.FindStringSubmatch(replacedLine); match != nil {
	//        // If we want to check against the friends cache, we use the email only matching group
	//        s := match[2]
	//        friends[s] = struct{}{}
	//        replacedLine = strings.Replace(replacedLine, s, " ", 1)
	//    }
	//}

	//return friends

	// Regex ops above (although far more accurate in terms of email matching) are _ridiculously_ expensive,
	// therefore we're going to (for now) default to basic matching of any words starting with "@"
	r.friendsUpdateLock.RLock()
	defer r.friendsUpdateLock.RUnlock()

	hasFoundFriend := false
	isOrdered := true
	friendsMap := map[string]struct{}{}

	// add existing friends, if there are any
	for _, f := range existingFriends {
		friendsMap[f] = struct{}{}
	}

	for _, w := range strings.Split(line, " ") {
		// We only force lower case when comparing to the friends cache, as we need to maintain case (for now)
		// for purposes of matching on the string for repositiong. The email is lower-cased after repositioning.
		if len(w) > 1 && rune(w[0]) == '@' && r.friends[strings.ToLower(w[1:])] != nil {
			// If there are duplicates, the line has not been processed
			if _, exists := friendsMap[w[1:]]; exists {
				isOrdered = false
			}
			friendsMap[w[1:]] = struct{}{}
			hasFoundFriend = true
		} else if hasFoundFriend {
			// If we reach here, the current word is _not_ a friend, but we have previously processed one
			isOrdered = false
		}
	}
	friends := []string{}
	for f := range friendsMap {
		friends = append(friends, f)
	}
	return friends, isOrdered
}

func (r *DBListRepo) getEmailFromConfigLine(line string) string {
	//if len(line) == 0 {
	//    return ""
	//}
	//match := r.cfgFriendRegex.FindStringSubmatch(line)
	//// First submatch is the email regex
	//if len(match) > 1 {
	//    return match[1]
	//}
	//return ""

	// Avoiding expensive regex based ops for now
	var f string
	if words := strings.Fields(line); len(words) == 3 && words[0] == "fzn_cfg:friend" && rune(words[2][0]) == '@' && words[2][1:] == r.email {
		f = strings.ToLower(words[1])
	}
	return f
}

func (r *DBListRepo) repositionActiveFriends(e EventLog) EventLog {
	// SL 2021-12-30: Recent changes mean that _all_ event logs will now store the current state of the line, so this
	// check is only relevant to bypass earlier logs which have nothing to process.
	if len(e.Line) == 0 {
		return e
	}

	if e.Friends.IsProcessed {
		return e
	}

	// If this is a config line, we only want to hide the owner email, therefore manually set a single key friends map
	var friends []string
	//if r.cfgFriendRegex.MatchString(e.Line) {
	if r.getEmailFromConfigLine(e.Line) != "" {
		friends = append(friends, r.email)
	} else {
		// Retrieve existing friends from the item, if it already exists. This prevents the following bug:
		// - Item is shared with `@foo@bar.com`
		// - `fzn_cfg:friend foo@bar.com` is removed
		// - Update is made on previously shared line
		// - `@foo@bar.com` is appended to the Line() portion of the string, because the friend is no longer present in
		//   the r.friends cache
		var existingFriends []string
		if item, exists := r.listItemTracker[e.ListItemKey]; exists {
			existingFriends = item.friends.Emails
		}
		// If there are no friends, return early
		friends, _ = r.getFriendsFromLine(e.Line, existingFriends)
		if len(friends) == 0 {
			e.Friends.IsProcessed = true
			e.Friends.Offset = len(e.Line)
			return e
		}
	}

	newLine := e.Line
	for _, f := range friends {
		atFriend := "@" + f
		// Cover edge case whereby email is typed first and constitutes entire string
		if atFriend == newLine {
			newLine = ""
		} else {
			// Each cut email address needs also remove (up to) one (pre|suf)fixed space, not more.
			newLine = strings.ReplaceAll(newLine, " "+atFriend, "")
			newLine = strings.ReplaceAll(newLine, atFriend+" ", "")
		}
	}

	friendString := ""

	// Sort the emails, and then append a space separated string to the end of the Line
	sort.Strings(friends)
	for i, f := range friends {
		friendString += " @"
		lowerF := strings.ToLower(f)
		friendString += lowerF
		friends[i] = lowerF // override the friends slice to ensure lower case
	}

	newLine += friendString
	e.Line = newLine

	e.Friends.IsProcessed = true
	e.Friends.Offset = len(newLine) - len(friendString)
	e.Friends.Emails = friends
	return e
}

func (r *DBListRepo) generateFriendChangeEvents(e EventLog, item *ListItem) {
	// This method is responsible for detecting changes to the "friends" configuration in order to update
	// local state, and to emit events to the cloud API.
	var friendToAdd, friendToRemove string

	var existingLine string
	if item != nil {
		existingLine = item.rawLine
	}

	before := r.getEmailFromConfigLine(existingLine)
	switch e.EventType {
	case AddEvent, UpdateEvent:
		after := r.getEmailFromConfigLine(e.Line)
		// If before != after, we know that we need to remove the previous entry, and add the new one.
		if before != after {
			friendToRemove = before
			friendToAdd = after
		}
	case DeleteEvent:
		friendToRemove = before
	default:
		return
	}

	if friendToRemove == "" && friendToAdd == "" {
		return
	}

	// We now iterate over each friend in the two slices and compare to the cached friends on DBListRepo.
	// If the friend doesn't exist, or the timestamp is more recent than the cached counterpart, we update.
	// We do this on a per-listItem basis to account for duplicate lines.
	func() {
		// We need both additions and deletions to be handled in an atomic fully blocking op, so the update events
		// aren't emitted with pending deletions still present
		r.friendsUpdateLock.Lock()
		defer r.friendsUpdateLock.Unlock()
		if friendToAdd != "" {
			// If the listItem specific friend exists, skip
			var friendItems map[string]int64
			var friendExists bool
			if friendItems, friendExists = r.friends[friendToAdd]; !friendExists {
				friendItems = make(map[string]int64)
				r.friends[friendToAdd] = friendItems
			}

			if dtLastChange, exists := friendItems[e.ListItemKey]; !exists || e.LamportTimestamp > dtLastChange {
				r.friends[friendToAdd][e.ListItemKey] = e.LamportTimestamp
				// TODO the consumer of the channel below will need to be responsible for adding the walfile locally
				r.AddWalFile(
					&WebWalFile{
						uuid: friendToAdd,
						web:  r.web,
					},
					false,
				)
			}
		}
		//for email := range friendsToRemove {
		if friendToRemove != "" && friendToRemove != r.email {
			// We only delete and emit the cloud event if the friend exists (which it always should tbf)
			// Although we ignore the delete if the event timestamp is older than the latest known cache state.
			if friendItems, friendExists := r.friends[friendToRemove]; friendExists {
				if dtLastChange, exists := friendItems[e.ListItemKey]; exists && e.LamportTimestamp > dtLastChange {
					delete(r.friends[friendToRemove], e.ListItemKey)
					if len(r.friends[friendToRemove]) == 0 {
						delete(r.friends, friendToRemove)
						r.DeleteWalFile(friendToRemove)
					}
				}
			}
		}
		if (friendToAdd != "" || friendToRemove != "") && r.friendsMostRecentChangeDT < e.LamportTimestamp {
			r.friendsMostRecentChangeDT = e.LamportTimestamp
		}
	}()
}

func (r *DBListRepo) processEventLog(e EventLog) (*ListItem, error) {
	item := r.listItemTracker[e.ListItemKey]
	targetItem := r.listItemTracker[e.TargetListItemKey]

	// Skip any events that have already been processed
	if _, exists := r.processedEventLogCache[e.key()]; exists {
		return item, nil
	}
	r.processedEventLogCache[e.key()] = struct{}{}

	// Else, skip any event of equal EventType that is <= the most recently processed for a given ListItem
	// TODO storing a whole EventLog might be expensive, use a specific/reduced type in the nested map
	if eventTypeCache, exists := r.listItemProcessedEventLogTypeCache[e.EventType]; exists {
		if ce, exists := eventTypeCache[e.ListItemKey]; exists {
			switch checkEquality(ce, e) {
			// if the new event is older or equal, skip
			case rightEventOlder, eventsEqual:
				return item, nil
			}
		}
	} else {
		r.listItemProcessedEventLogTypeCache[e.EventType] = make(map[string]EventLog)
	}
	r.listItemProcessedEventLogTypeCache[e.EventType][e.ListItemKey] = e

	if r.currentLamportTimestamp <= e.LamportTimestamp {
		r.currentLamportTimestamp = e.LamportTimestamp
	}

	// We need to maintain records of deleted items in the cache, but if deleted, want to assign nil ptrs
	// in the various funcs below, so set to nil
	if item != nil && item.isDeleted {
		item = nil
	}
	if targetItem != nil && targetItem.isDeleted {
		targetItem = nil
	}

	// When we're calling this function on initial WAL merge and load, we may come across
	// orphaned items. There MIGHT be a valid case to keep events around if the EventType
	// is Update. Item will obviously never exist for Add. For all other eventTypes,
	// we should just skip the event and return
	// TODO remove this AddEvent nil item passthrough
	if item == nil && e.EventType != AddEvent && e.EventType != UpdateEvent {
		return item, nil
	}

	r.generateFriendChangeEvents(e, item)

	var err error
	switch e.EventType {
	case AddEvent:
		if item != nil {
			// 21/11/21: There was a bug caused when a collaborator `Undo` was carried out on a collaborator origin
			// `Delete`, when the `Delete` was not picked up by the local. This resulted in an `Add` being carried out
			// with a duplicate ListItem key, which led to some F'd up behaviour in the match set. This catch covers
			// this case by doing a dumb "if Add on existing item, change to Update". We need to run two updates, as
			// Note and Line updates are individual operations.
			// TODO remove this when `Compact`/wal post-processing is smart enough to iron out these broken logs.
			err = r.update(item, e.Line, e.Friends, e.Note)
			err = r.update(item, "", e.Friends, e.Note)
		} else {
			item, err = r.add(e.ListItemKey, e.Line, e.Friends, e.Note, targetItem)
		}
	case UpdateEvent:
		// We have to cover an edge case here which occurs when merging two remote WALs. If the following occurs:
		// - wal1 creates item A
		// - wal2 copies wal1
		// - wal2 deletes item A
		// - wal1 updates item A
		// - wal1 copies wal2
		// We will end up with an attempted Update on a nonexistent item.
		// In this case, we will Add an item back in with the updated content
		// NOTE A side effect of this will be that the re-added item will be at the top of the list as it
		// becomes tricky to deal with child IDs
		if item != nil {
			err = r.update(item, e.Line, e.Friends, e.Note)
		} else {
			item, err = r.add(e.ListItemKey, e.Line, e.Friends, e.Note, targetItem)
		}
	case MoveDownEvent:
		if targetItem == nil {
			return item, nil
		}
		fallthrough
	case MoveUpEvent:
		item, err = r.move(item, targetItem)
	case ShowEvent:
		err = r.setVisibility(item, true)
	case HideEvent:
		err = r.setVisibility(item, false)
	case DeleteEvent:
		err = r.del(item)
	}

	if item != nil {
		r.listItemTracker[e.ListItemKey] = item
	}

	return item, err
}

func (r *DBListRepo) add(key string, line string, friends LineFriends, note []byte, childItem *ListItem) (*ListItem, error) {
	newItem := &ListItem{
		key:        key,
		child:      childItem,
		rawLine:    line,
		Note:       note,
		friends:    friends,
		localEmail: r.email,
	}

	// If `child` is nil, it's the first item in the list so set as root and return
	if childItem == nil {
		oldRoot := r.Root
		r.Root = newItem
		if oldRoot != nil {
			newItem.parent = oldRoot
			oldRoot.child = newItem
		}
		return newItem, nil
	}

	if childItem.parent != nil {
		childItem.parent.child = newItem
		newItem.parent = childItem.parent
	}
	childItem.parent = newItem

	return newItem, nil
}

func (r *DBListRepo) update(item *ListItem, line string, friends LineFriends, note []byte) error {
	if len(line) > 0 {
		item.rawLine = line
		// item.friends.emails is a map, which we only ever want to OR with to aggregate
		mergedEmailMap := make(map[string]struct{})
		for _, e := range item.friends.Emails {
			mergedEmailMap[e] = struct{}{}
		}
		for _, e := range friends.Emails {
			mergedEmailMap[e] = struct{}{}
		}
		item.friends.IsProcessed = friends.IsProcessed
		item.friends.Offset = friends.Offset
		emails := []string{}
		for e := range mergedEmailMap {
			if e != r.email {
				emails = append(emails, e)
			}
		}
		sort.Strings(emails)
		item.friends.Emails = emails
	} else {
		item.Note = note
	}

	// Just in case an Update occurs on a Deleted item (distributed race conditions)
	item.isDeleted = false

	return nil
}

func (r *DBListRepo) del(item *ListItem) error {
	item.isDeleted = true

	if item.child != nil {
		item.child.parent = item.parent
	} else {
		// If the item has no child, it is at the top of the list and therefore we need to update the root
		r.Root = item.parent
	}

	if item.parent != nil {
		item.parent.child = item.child
	}

	return nil
}

func (r *DBListRepo) move(item *ListItem, childItem *ListItem) (*ListItem, error) {
	var err error
	err = r.del(item)
	isHidden := item.IsHidden
	item, err = r.add(item.key, item.rawLine, item.friends, item.Note, childItem)
	if isHidden {
		r.setVisibility(item, false)
	}
	return item, err
}

func (r *DBListRepo) setVisibility(item *ListItem, isVisible bool) error {
	item.IsHidden = !isVisible
	return nil
}

// Replay updates listItems based on the current state of the local WAL logs. It generates or updates the linked list
// which is attached to DBListRepo.Root
func (r *DBListRepo) Replay(partialWal []EventLog) error {
	// No point merging with an empty partialWal
	if len(partialWal) == 0 {
		return nil
	}

	for _, e := range partialWal {
		r.processEventLog(e)
	}

	r.log = merge(r.log, partialWal)

	return nil
}

func getNextEventLogFromWalFile(r io.Reader, schemaVersionID uint16) (*EventLog, error) {
	el := EventLog{}

	switch schemaVersionID {
	case 3:
		wi := walItemSchema2{}
		err := binary.Read(r, binary.LittleEndian, &wi)
		if err != nil {
			return nil, err
		}

		el.UUID = wi.UUID
		el.EventType = wi.EventType
		el.ListItemKey = strconv.Itoa(int(wi.UUID)) + ":" + strconv.Itoa(int(wi.ListItemCreationTime))
		el.TargetListItemKey = strconv.Itoa(int(wi.TargetUUID)) + ":" + strconv.Itoa(int(wi.TargetListItemCreationTime))
		el.LamportTimestamp = wi.EventTime

		line := make([]byte, wi.LineLength)
		err = binary.Read(r, binary.LittleEndian, &line)
		if err != nil {
			return nil, err
		}
		el.Line = string(line)

		if wi.NoteExists {
			el.Note = []byte{}
		}
		if wi.NoteLength > 0 {
			note := make([]byte, wi.NoteLength)
			err = binary.Read(r, binary.LittleEndian, &note)
			if err != nil {
				return nil, err
			}
			el.Note = note
		}
	default:
		return nil, errors.New("unrecognised wal schema version")
	}
	return &el, nil
}

func getOldEventLogKeys(i interface{}) (string, string) {
	var key, targetKey string
	switch e := i.(type) {
	case EventLogSchema4:
		key = strconv.Itoa(int(e.UUID)) + ":" + strconv.Itoa(int(e.ListItemCreationTime))
		targetKey = strconv.Itoa(int(e.TargetUUID)) + ":" + strconv.Itoa(int(e.TargetListItemCreationTime))
	case EventLogSchema5:
		key = strconv.Itoa(int(e.UUID)) + ":" + strconv.Itoa(int(e.ListItemCreationTime))
		targetKey = strconv.Itoa(int(e.TargetUUID)) + ":" + strconv.Itoa(int(e.TargetListItemCreationTime))
	}
	return key, targetKey
}

func buildFromFile(raw io.Reader) ([]EventLog, error) {
	var el []EventLog
	var walSchemaVersionID uint16
	if err := binary.Read(raw, binary.LittleEndian, &walSchemaVersionID); err != nil {
		if err == io.EOF {
			return el, nil
		}
		return el, err
	}

	pr, pw := io.Pipe()
	errChan := make(chan error, 1)
	go func() {
		defer pw.Close()
		switch walSchemaVersionID {
		case 1, 2:
			if _, err := io.Copy(pw, raw); err != nil {
				errChan <- err
			}
		default:
			// Versions >=3 of the wal schema is gzipped after the first 2 bytes. Therefore, unzip those bytes
			// prior to passing it to the loop below
			zr, err := gzip.NewReader(raw)
			if err != nil {
				errChan <- err
			}
			defer zr.Close()
			if _, err := io.Copy(pw, zr); err != nil {
				errChan <- err
			}
		}
		errChan <- nil
	}()

	switch walSchemaVersionID {
	case 1, 2, 3:
		for {
			select {
			case err := <-errChan:
				if err != nil {
					return el, err
				}
			default:
				e, err := getNextEventLogFromWalFile(pr, walSchemaVersionID)
				if err != nil {
					switch err {
					case io.EOF:
						return el, nil
					case io.ErrUnexpectedEOF:
						// Given the distributed concurrent nature of this app, we sometimes pick up partially
						// uploaded files which will fail, but may well be complete later on, therefore just
						// return for now and attempt again later
						// TODO implement a decent retry mech here
						return el, nil
					default:
						return el, err
					}
				}
				el = append(el, *e)
			}
		}
	case 4:
		var oel []EventLogSchema4
		dec := gob.NewDecoder(pr)
		if err := dec.Decode(&oel); err != nil {
			return el, err
		}
		if err := <-errChan; err != nil {
			return el, err
		}
		for _, oe := range oel {
			key, targetKey := getOldEventLogKeys(oe)
			e := EventLog{
				UUID:              oe.UUID,
				ListItemKey:       key,
				TargetListItemKey: targetKey,
				LamportTimestamp:  oe.UnixNanoTime,
				EventType:         oe.EventType,
				Line:              oe.Line,
				Note:              oe.Note,
				Friends: LineFriends{
					IsProcessed: oe.Friends.IsProcessed,
					Offset:      oe.Friends.Offset,
					emailsMap:   oe.Friends.Emails,
				},
			}
			for f := range oe.Friends.Emails {
				e.Friends.Emails = append(e.Friends.Emails, f)
			}
			sort.Strings(e.Friends.Emails)
			el = append(el, e)
		}
	case 5:
		var oel []EventLogSchema5
		dec := gob.NewDecoder(pr)
		if err := dec.Decode(&oel); err != nil {
			return el, err
		}
		if err := <-errChan; err != nil {
			return el, err
		}
		for _, oe := range oel {
			key, targetKey := getOldEventLogKeys(oe)
			e := EventLog{
				UUID:              oe.UUID,
				ListItemKey:       key,
				TargetListItemKey: targetKey,
				LamportTimestamp:  oe.UnixNanoTime,
				EventType:         oe.EventType,
				Line:              oe.Line,
				Note:              oe.Note,
				Friends:           oe.Friends,
			}
			el = append(el, e)
		}
	case 6:
		dec := gob.NewDecoder(pr)
		if err := dec.Decode(&el); err != nil {
			return el, err
		}
		if err := <-errChan; err != nil {
			return el, err
		}
	}

	return el, nil
}

const (
	eventsEqual int = iota
	leftEventOlder
	rightEventOlder
)

func checkEquality(event1 EventLog, event2 EventLog) int {
	if event1.LamportTimestamp < event2.LamportTimestamp ||
		event1.LamportTimestamp == event2.LamportTimestamp && event1.UUID < event2.UUID {
		return leftEventOlder
	} else if event2.LamportTimestamp < event1.LamportTimestamp ||
		event2.LamportTimestamp == event1.LamportTimestamp && event2.UUID < event1.UUID {
		return rightEventOlder
	}
	return eventsEqual
}

func merge(wal1 []EventLog, wal2 []EventLog) []EventLog {
	if len(wal1) == 0 && len(wal2) == 0 {
		return []EventLog{}
	} else if len(wal1) == 0 {
		return wal2
	} else if len(wal2) == 0 {
		return wal1
	}

	// Pre-allocate a slice with the maximum possible items (sum of both lens). Although under many circumstances, it's
	// unlikely we'll fill the capacity, it's far more optimal than each separate append re-allocating to a new slice.
	mergedEl := make([]EventLog, 0, len(wal1)+len(wal2))

	// Before merging, check to see that the the most recent from one wal isn't older than the oldest from another.
	// If that is the case, append the newer to the older and return.
	// We append to the newly allocated mergedEl twice, as we can guarantee that the underlying capacity will be enough
	// (so no further allocations are needed)
	if checkEquality(wal1[0], wal2[len(wal2)-1]) == rightEventOlder {
		mergedEl = append(mergedEl, wal2...)
		mergedEl = append(mergedEl, wal1...)
		return mergedEl
	} else if checkEquality(wal2[0], wal1[len(wal1)-1]) == rightEventOlder {
		mergedEl = append(mergedEl, wal1...)
		mergedEl = append(mergedEl, wal2...)
		return mergedEl
	}

	// Adopt a two pointer approach
	i, j := 0, 0
	// We can use an empty log here because it will never be equal to in the checkEquality calls below
	lastEvent := EventLog{}
	for i < len(wal1) || j < len(wal2) {
		if len(mergedEl) > 0 {
			lastEvent = mergedEl[len(mergedEl)-1]
		}
		if i == len(wal1) {
			// Ignore duplicates (compare with current head of the array
			if len(mergedEl) == 0 || checkEquality(wal2[j], lastEvent) != eventsEqual {
				mergedEl = append(mergedEl, wal2[j])
			}
			j++
		} else if j == len(wal2) {
			// Ignore duplicates (compare with current head of the array
			if len(mergedEl) == 0 || checkEquality(wal1[i], lastEvent) != eventsEqual {
				mergedEl = append(mergedEl, wal1[i])
			}
			i++
		} else {
			switch checkEquality(wal1[i], wal2[j]) {
			case leftEventOlder:
				if len(mergedEl) == 0 || checkEquality(wal1[i], lastEvent) != eventsEqual {
					mergedEl = append(mergedEl, wal1[i])
				}
				i++
			case rightEventOlder:
				if len(mergedEl) == 0 || checkEquality(wal2[j], lastEvent) != eventsEqual {
					mergedEl = append(mergedEl, wal2[j])
				}
				j++
			case eventsEqual:
				// At this point, we only want to guarantee an increment on ONE of the two pointers
				if i < len(wal1) {
					i++
				} else {
					j++
				}
			}
		}
	}
	return mergedEl
}

func areListItemsEqual(a *ListItem, b *ListItem, checkPointers bool) bool {
	// checkPointers prevents recursion
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.rawLine != b.rawLine ||
		string(a.Note) != string(b.Note) ||
		a.IsHidden != b.IsHidden ||
		a.key != b.key {
		return false
	}
	if checkPointers {
		if !areListItemsEqual(a.child, b.child, false) ||
			!areListItemsEqual(a.parent, b.parent, false) ||
			!areListItemsEqual(a.matchChild, b.matchChild, false) ||
			!areListItemsEqual(a.matchParent, b.matchParent, false) {
			return false
		}
	}
	return true
}

func checkListItemPtrs(listItem *ListItem, matchItems []ListItem) error {
	if listItem == nil {
		return nil
	}

	if listItem.child != nil {
		return errors.New("list integrity error: root has a child pointer")
	}

	i := 0
	processedItems := make(map[string]struct{})
	var prev *ListItem
	for listItem.parent != nil {
		// Ensure current.child points to the previous
		// TODO check if this is just duplicating a check in areListItemsEqual below
		if listItem.child != prev {
			return fmt.Errorf("list integrity error: listItem %s child ptr does not point to the expected item", listItem.key)
		}

		if !areListItemsEqual(listItem, &matchItems[i], false) {
			return fmt.Errorf("list integrity error: listItem %s does not match the expected position in the match list", listItem.key)
		}

		if _, exists := processedItems[listItem.key]; exists {
			return fmt.Errorf("list integrity error: listItem %s has appeared twice", listItem.key)
		}

		processedItems[listItem.key] = struct{}{}
		prev = listItem
		listItem = listItem.parent
		i++
	}

	// Check to see if there are remaining items in the match list
	// NOTE: `i` will have been incremented one more time beyond the final tested index
	if i+1 < len(matchItems) {
		return errors.New("list integrity error: orphaned items in match set")
	}

	return nil
}

// listsAreEquivalent traverses both test generated list item linked lists (from full and compacted wals) to
// check for equality. It returns `true` if they they are identical lists, or `false` otherwise.
// This is primarily used as a temporary measure to check the correctness of the compaction algo
func listsAreEquivalent(ptrA *ListItem, ptrB *ListItem) bool {
	// Return false if only one is nil
	if (ptrA != nil && ptrB == nil) || (ptrA == nil && ptrB != nil) {
		return false
	}

	// Check root equality
	if !areListItemsEqual(ptrA, ptrB, true) {
		return false
	}

	// Return true if both are nil (areListItemsEqual returns true if both nil so only check one)
	if ptrA == nil {
		return true
	}

	// Iterate over both ll's together and check equality of each item. Return `false` as soon as a pair
	// don't match, or one list is a different length to another
	for ptrA.parent != nil && ptrB.parent != nil {
		ptrA = ptrA.parent
		ptrB = ptrB.parent
		if !areListItemsEqual(ptrA, ptrB, true) {
			return false
		}
	}

	return areListItemsEqual(ptrA, ptrB, true)
}

// NOTE: not in use - debug function
func writePlainWalToFile(wal []EventLog) {
	f, err := os.Create(fmt.Sprintf("debug_%d", time.Now().UnixNano()))
	if err != nil {
		fmt.Println(err)
		f.Close()
		return
	}
	defer f.Close()

	for _, e := range wal {
		fmt.Fprintln(f, e)
		if err != nil {
			fmt.Println(err)
			return
		}
	}
}

func checkWalIntegrity(wal []EventLog) (*ListItem, []ListItem, error) {
	// Generate a test repo and use it to generate a match set, then inspect the health
	// of said match set.
	testRepo := DBListRepo{
		log:             []EventLog{},
		listItemTracker: make(map[string]*ListItem),

		webWalFiles:    make(map[string]WalFile),
		allWalFiles:    make(map[string]WalFile),
		syncWalFiles:   make(map[string]WalFile),
		webWalFileMut:  &sync.RWMutex{},
		allWalFileMut:  &sync.RWMutex{},
		syncWalFileMut: &sync.RWMutex{},

		friends:           make(map[string]map[string]int64),
		friendsUpdateLock: &sync.RWMutex{},

		processedWalChecksums:    make(map[string]struct{}),
		processedWalChecksumLock: &sync.Mutex{},
	}

	// Use the Replay function to generate the linked lists
	if err := testRepo.Replay(wal); err != nil {
		return nil, nil, err
	}

	// 22/11/21: The Match() function usually returns a slice of ListItem copies as the first argument, which is fine for
	// client operation, as we only reference them for the attached Lines/Notes (and any mutations act on a different
	// slice of ListItem ptrs).
	// In order to allow us to check the state of the ListItems more explicitly, we reference the testRepo.matchListItems
	// which are generated in parallel on Match() calls, as we can then check proper equality between the various items.
	// This isn't ideal as it's not completely consistent with what's returned to the client, but it's a reasonable check
	// for now (and I intend on centralising the logic anyway so we don't maintain two versions of state).
	// TODO, remove this comment when logic is centralised
	matchListItems, _, err := testRepo.Match([][]rune{}, true, "", 0, 0)
	if err != nil {
		return nil, nil, errors.New("failed to generate match items for list integrity check")
	}

	// This check traverses from the root node to the last parent and checks the state of the pointer
	// relationships between both. There have previously been edge case wal merge/compaction bugs which resulted
	// in MoveUp events targeting a child, who's child was the original item to be moved (a cyclic pointer bug).
	// This has since been fixed, but to catch other potential cases, we run this check.
	if err := checkListItemPtrs(testRepo.Root, matchListItems); err != nil {
		return nil, matchListItems, err
	}

	return testRepo.Root, matchListItems, nil
}

// recoverWal is responsible for recovering Wals that are very broken, in a variety of ways.
// It collects as much metadata about each item as it can, from both the existing broken wal and the returned
// match set, and then uses it to rebuild a fresh wal, maintaining as much of the original state as possible -
// specifically with regards to DTs and ordering.
func (r *DBListRepo) recoverWal(wal []EventLog, matches []ListItem) []EventLog {
	acknowledgedItems := make(map[string]ListItem)
	listOrder := []string{}

	// Iterate over the match items and add all to the map
	for _, item := range matches {
		key := item.key
		if _, exists := acknowledgedItems[key]; !exists {
			listOrder = append(listOrder, key)
		}
		acknowledgedItems[key] = item
	}

	// Iterate over the wal and collect data, updating the items in the map as we go.
	// We update metadata based on Updates only (Move*s are unimportant here). We honour deletes, and will remove
	// then from the map (although any subsequent updates will put them back in)
	// This cleanup will also dedup Add events if there are cases with duplicates.
	for _, e := range wal {
		item, exists := acknowledgedItems[e.ListItemKey]
		switch e.EventType {
		case AddEvent:
			fallthrough
		case UpdateEvent:
			if !exists {
				item = ListItem{
					rawLine: e.Line,
					Note:    e.Note,
					key:     e.ListItemKey,
				}
				if e.ListItemKey != item.key {
					log.Fatal("ListItem key mismatch during recovery")
				}
			} else {
				if e.EventType == AddEvent {
					item.rawLine = e.Line
					item.Note = e.Note
				} else {
					// Updates handle Line and Note mutations separately
					if e.Note != nil {
						item.Note = e.Note
					} else {
						item.rawLine = e.Line
					}
				}
			}
			acknowledgedItems[e.ListItemKey] = item
		case HideEvent:
			if exists {
				item.IsHidden = true
				acknowledgedItems[e.ListItemKey] = item
			}
		case ShowEvent:
			if exists {
				item.IsHidden = false
				acknowledgedItems[e.ListItemKey] = item
			}
		case DeleteEvent:
			delete(acknowledgedItems, e.ListItemKey)
		}
	}

	// Now, iterate over the ordered list of item keys in reverse order, and pull the item from the map,
	// generating an AddEvent for each.
	newWal := []EventLog{}
	for i := len(listOrder) - 1; i >= 0; i-- {
		item := acknowledgedItems[listOrder[i]]
		el := EventLog{
			EventType:   AddEvent,
			ListItemKey: item.key,
			Line:        item.rawLine,
			Note:        item.Note,
		}
		newWal = append(newWal, el)

		if item.IsHidden {
			r.currentLamportTimestamp++
			el := EventLog{
				EventType:        HideEvent,
				LamportTimestamp: r.currentLamportTimestamp,
				ListItemKey:      item.key,
			}
			newWal = append(newWal, el)
		}
	}

	return newWal
}

func reorderWal(wal []EventLog) []EventLog {
	sort.Slice(wal, func(i, j int) bool {
		return checkEquality(wal[i], wal[j]) == leftEventOlder
	})
	return wal
}

func (r *DBListRepo) compact(wal []EventLog) ([]EventLog, error) {
	if len(wal) == 0 {
		return []EventLog{}, nil
	}
	// TODO remove both of these checks when(/if) the compact algo becomes bulletproof
	// We re-order the wal in case any historical bugs caused the log to become
	// out of order
	sort.Slice(wal, func(i, j int) bool {
		return checkEquality(wal[i], wal[j]) == leftEventOlder
	})

	// Check the integrity of the incoming full wal prior to compaction.
	// If broken in some way, call the recovery function.
	testRootA, matchItemsA, err := checkWalIntegrity(wal)
	if err != nil {
		// If we get here, shit's on fire. This function is the equivalent of the fire brigade.
		wal = r.recoverWal(wal, matchItemsA)
		testRootA, _, err = checkWalIntegrity(wal)
		if err != nil {
			log.Fatal("wal recovery failed!")
		}

		return wal, errWalIntregrity
	}

	// Traverse from most recent to most distant logs. Omit events in the following scenarios:
	// NOTE delete event purging is currently disabled
	// - Delete events, and any events preceding a DeleteEvent
	// - Update events in the following circumstances
	//   - Any UpdateEvent with a Note preceding the most recent UpdateEvent with a Note
	//   - Same without a Note
	//
	// Opting to store all Move* events to maintain the most consistent ordering of the output linked list.
	// e.g. it'll attempt to apply oldest -> newest Move*s until the target pointers don't exist.
	//
	// We need to maintain the first of two types of Update events (as per above, separate Line and Note),
	// so generate a separate set for each to tell us if each has occurred
	updateWithNote := make(map[string]struct{})
	updateWithLine := make(map[string]struct{})

	compactedWal := []EventLog{}
	for i := len(wal) - 1; i >= 0; i-- {
		e := wal[i]

		// TODO figure out how to reintegrate full purge of deleted events, whilst guaranteeing consistent
		// state of ListItems. OR purge everything older than X days, so ordering doesn't matter cos users
		// won't see it??
		// Add DeleteEvents straight to the purge set, if there's not any newer update events
		// NOTE it's important that we only continue if ONE OF BOTH UPDATE TYPES IS ALREADY PROCESSED
		//if e.EventType == DeleteEvent {
		//    if _, noteExists := updateWithNote[e.ListItemKey]; !noteExists {
		//        if _, lineExists := updateWithLine[e.ListItemKey]; !lineExists {
		//            keysToPurge[e.ListItemKey] = struct{}{}
		//            continue
		//        }
		//    }
		//}

		if e.EventType == UpdateEvent {
			//Check to see if the UpdateEvent alternative event has occurred
			// Nil `Note` signifies `Line` update
			//if e.Note != nil {
			//    if _, exists := updateWithNote[e.ListItemKey]; exists {
			//        continue
			//    }
			//    updateWithNote[e.ListItemKey] = struct{}{}
			//} else {
			//    if _, exists := updateWithLine[e.ListItemKey]; exists {
			//        continue
			//    }
			//    updateWithLine[e.ListItemKey] = struct{}{}
			//}

			if len(e.Line) > 0 {
				if _, exists := updateWithLine[e.ListItemKey]; exists {
					continue
				}
				updateWithLine[e.ListItemKey] = struct{}{}
			} else {
				if _, exists := updateWithNote[e.ListItemKey]; exists {
					continue
				}
				updateWithNote[e.ListItemKey] = struct{}{}
			}
		}

		compactedWal = append(compactedWal, e)
	}
	// Reverse
	for i, j := 0, len(compactedWal)-1; i < j; i, j = i+1, j-1 {
		compactedWal[i], compactedWal[j] = compactedWal[j], compactedWal[i]
	}

	// TODO remove this once confidence with compact is there!
	// This is a circuit breaker which will blow up if compact generates inconsistent results
	testRootB, _, err := checkWalIntegrity(compactedWal)
	if err != nil {
		log.Fatalf("`compact` caused wal to lose integrity: %s", err)
	}

	if !listsAreEquivalent(testRootA, testRootB) {
		//sliceA := []ListItem{}
		//node := testRootA
		//for node != nil {
		//    sliceA = append(sliceA, *node)
		//    node = node.parent
		//}
		//generatePlainTextFile(sliceA)

		//sliceB := []ListItem{}
		//node = testRootB
		//for node != nil {
		//    sliceB = append(sliceB, *node)
		//    node = node.parent
		//}
		//generatePlainTextFile(sliceB)

		//return wal, nil
		log.Fatal("`compact` generated inconsistent results and things blew up!")
	}
	return compactedWal, nil
}

// generatePlainTextFile takes the current matchset, and writes the lines separately to a
// local file. Notes are ignored.
func generatePlainTextFile(matchItems []ListItem) error {
	curWd, err := os.Getwd()
	if err != nil {
		return err
	}
	// Will be in the form `{currentDirectory}/export_1624785401.txt`
	fileName := path.Join(curWd, fmt.Sprintf(exportFilePattern, time.Now().UnixNano()))
	f, err := os.Create(fileName)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	for _, i := range matchItems {
		if _, err := f.Write([]byte(i.rawLine + "\n")); err != nil {
			return err
		}
	}
	return nil
}

// This function is currently unused
//func (r *DBListRepo) generatePartialView(ctx context.Context, matchItems []ListItem) error {
//    wal := []EventLog{}
//    //now := time.Now().AddDate(-1, 0, 0).UnixNano()
//    now := int64(1) // TODO remove this - it's to ensure consistency to enable file diffs

//    // Iterate from oldest to youngest
//    for i := len(matchItems) - 1; i >= 0; i-- {
//        item := matchItems[i]
//        el := EventLog{
//            UUID:                       item.originUUID,
//            TargetUUID:                 0,
//            ListItemCreationTime:       item.creationTime,
//            TargetListItemCreationTime: 0,
//            UnixNanoTime:               now,
//            EventType:                  AddEvent,
//            Line:                       item.rawLine,
//            Note:                       item.Note,
//        }
//        wal = append(wal, el)
//        now++

//        if item.IsHidden {
//            el.EventType = HideEvent
//            el.UnixNanoTime = now
//            wal = append(wal, el)
//            now++
//        }
//    }

//    b, _ := buildByteWal(wal)
//    viewName := fmt.Sprintf(viewFilePattern, time.Now().UnixNano())
//    r.LocalWalFile.Flush(ctx, b, viewName)
//    log.Fatalf("N list generated events: %d", len(wal))
//    return nil
//}

func (r *DBListRepo) setProcessedWalChecksum(checksum string) {
	r.processedWalChecksumLock.Lock()
	defer r.processedWalChecksumLock.Unlock()
	r.processedWalChecksums[checksum] = struct{}{}
}

func (r *DBListRepo) isWalChecksumProcessed(checksum string) bool {
	r.processedWalChecksumLock.Lock()
	defer r.processedWalChecksumLock.Unlock()
	_, exists := r.processedWalChecksums[checksum]
	return exists
}

type walfileFilenamesJob struct {
	wf        WalFile
	filenames []string
}

type logFilenameJob struct {
	el       []EventLog
	filename string
}

func (r *DBListRepo) pull(ctx context.Context, walfiles <-chan WalFile) ([]EventLog, error) {
	walfileFilenameChan := make(chan walfileFilenamesJob, 5) // can't `len` walfiles gen, so just select an arbitrary buffer size

	go func() {
		var wg sync.WaitGroup
		defer close(walfileFilenameChan)
		for wf := range walfiles {
			wg.Add(1)
			go func(wf WalFile) {
				defer wg.Done()
				filePathPattern := path.Join(wf.GetRoot(), "wal_*.db")
				newWals, err := wf.GetMatchingWals(ctx, filePathPattern)
				if err != nil {
					return
				}
				walfileFilenameChan <- walfileFilenamesJob{
					wf:        wf,
					filenames: newWals,
				}
			}(wf)
		}
		wg.Wait()
	}()

	mergedWal := []EventLog{}
	filenamesToAck := []string{}
	for w := range walfileFilenameChan {
		walfileChan := make(chan logFilenameJob, len(w.filenames)) // all wals for each walfile
		go func() {
			defer close(walfileChan)
			for _, filename := range w.filenames {
				var newWfWal []EventLog
				if !r.isWalChecksumProcessed(filename) {
					pr, pw := io.Pipe()
					go func() {
						defer pw.Close()
						if err := w.wf.GetWalBytes(ctx, pw, filename); err != nil {
							// TODO handle
						}
					}()

					// Build new wals
					var err error
					newWfWal, err = buildFromFile(pr)
					if err != nil {
						// Ignore incompatible files
						continue
					}
				}
				// We send to walfileChan regardless of prior processing (an empty []EventLog is
				// passed if r.isWalChecksumProcessed resolves to true) as we may need to carry
				// out a `gather` in the next step. Empty []EventLogs are ignored in the merge step.
				walfileChan <- logFilenameJob{
					el:       newWfWal,
					filename: filename,
				}
			}
		}()
		walfileWal := []EventLog{}
		walfileFilesToDelete := make(map[string]struct{})
		for l := range walfileChan {
			walfileWal = merge(walfileWal, l.el)
			walfileFilesToDelete[l.filename] = struct{}{}
			filenamesToAck = append(filenamesToAck, l.filename)
		}
		mergedWal = merge(mergedWal, walfileWal)
		if len(w.filenames) > gatherFileNumberThreshold && r.isSyncWalfile(w.wf) {
			// Sometimes, we would have already acknowledged all of the wiles in the walfile, and therefore
			// the resultant event log will be empty. Therefore we need to merge with local r.log prior to
			// pushing it, to ensure we flush the full log
			walfileWal = merge(r.log, walfileWal)

			checksum, err := r.push(ctx, w.wf, walfileWal, nil)
			if err != nil {
				// Definitely don't delete pre-existing wals if the push fails for whatever reason
				return nil, err
			}
			r.setProcessedWalChecksum(checksum)

			// TODO: we are generating partial wals from the FULL local log at this point, to all non-owned
			// walfiles, on each separate gather. This is a duplication of effort and will have to be optimised in
			// the future
			for wf := range r.walFileGen(walFileCategoryNotOwned) {
				r.push(ctx, wf, walfileWal, nil)
			}

			// There's a chance that the new generated log will be identical to one that was already in the
			// remote walfile. Therefore, we omit that checksum from the delete set prior to sending to RemoveWals
			delete(walfileFilesToDelete, checksum)
			checksumOmitted := []string{}
			for filename := range walfileFilesToDelete {
				checksumOmitted = append(checksumOmitted, filename)
			}
			// Schedule a delete on the files
			w.wf.RemoveWals(ctx, checksumOmitted)
		}
	}

	for _, filename := range filenamesToAck {
		// Add to the processed cache after we've successfully pulled it
		r.setProcessedWalChecksum(filename)
	}

	return mergedWal, nil
}

func buildByteWal(el []EventLog) (*bytes.Buffer, error) {
	var outputBuf bytes.Buffer

	// Write the schema ID
	if err := binary.Write(&outputBuf, binary.LittleEndian, latestWalSchemaID); err != nil {
		return nil, err
	}

	// We need to encode the eventLog separately in order to generate a checksum
	var elBuf bytes.Buffer
	enc := gob.NewEncoder(&elBuf)
	if err := enc.Encode(el); err != nil {
		return nil, err
	}

	// Then write in the compressed bytes
	zw := gzip.NewWriter(&outputBuf)
	if _, err := zw.Write(elBuf.Bytes()); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}

	return &outputBuf, nil
}

func (r *DBListRepo) getMatchedWal(el []EventLog, wf WalFile) []EventLog {
	walFileOwnerEmail := wf.GetUUID()
	_, isWebRemote := wf.(*WebWalFile)
	isWalFileOwner := !isWebRemote || (r.email != "" && r.email == walFileOwnerEmail)

	// Only include those events which are/have been shared (this is handled via the event processed
	// cache elsewhere)
	filteredWal := []EventLog{}
	for _, e := range el {
		// Separate conditional here to prevent the need for e.getKeys lookup if not necessary
		if isWalFileOwner {
			filteredWal = append(filteredWal, e)
			continue
		}
		if e.emailHasAccess(walFileOwnerEmail) {
			filteredWal = append(filteredWal, e)
		}
	}
	return filteredWal
}

func (r *DBListRepo) push(ctx context.Context, wf WalFile, el []EventLog, byteWal *bytes.Buffer) (string, error) {
	if byteWal == nil {
		// Apply any filtering based on Push match configuration
		el = r.getMatchedWal(el, wf)

		// Return for empty wals
		if len(el) == 0 {
			return "", nil
		}

		var err error
		if byteWal, err = buildByteWal(el); err != nil {
			return "", err
		}

	}
	checksum := fmt.Sprintf("%x", md5.Sum(byteWal.Bytes()))

	// Add it straight to the cache to avoid processing it in the future
	// This needs to be done PRIOR to flushing to avoid race conditions
	// (as pull is done in a separate thread of control, and therefore we might try
	// and pull our own pushed wal)
	r.setProcessedWalChecksum(checksum)
	if err := wf.Flush(ctx, byteWal, checksum); err != nil {
		return checksum, err
	}

	return checksum, nil
}

func (r *DBListRepo) flushPartialWals(ctx context.Context, wal []EventLog, waitForCompletion bool) {
	//log.Print("Flushing...")
	if len(wal) > 0 {
		fullByteWal, err := buildByteWal(wal)
		if err != nil {
			return
		}
		var wg sync.WaitGroup
		r.allWalFileMut.RLock()
		defer r.allWalFileMut.RUnlock()
		for _, wf := range r.allWalFiles {
			if waitForCompletion {
				wg.Add(1)
			}
			var byteWal *bytes.Buffer
			if _, isOwned := r.syncWalFiles[wf.GetUUID()]; isOwned {
				byteWal = fullByteWal
			}
			go func(wf WalFile) {
				if waitForCompletion {
					defer wg.Done()
				}
				r.push(ctx, wf, wal, byteWal)
			}(wf)
		}
		if waitForCompletion {
			wg.Wait()
			os.Exit(0)
		}
	}
}

func (r *DBListRepo) emitRemoteUpdate() {
	if r.web.isActive {
		// We need to wrap the friendsMostRecentChangeDT comparison check, as the friend map update
		// and subsequent friendsMostRecentChangeDT update needs to be an atomic operation
		r.friendsUpdateLock.RLock()
		defer r.friendsUpdateLock.RUnlock()
		if r.friendsLastPushDT == 0 || r.friendsLastPushDT < r.friendsMostRecentChangeDT {
			u, _ := url.Parse(apiURL)
			u.Path = path.Join(u.Path, "remote")

			emails := []string{}
			for e := range r.friends {
				if e != r.email {
					emails = append(emails, e)
				}
			}

			remote := WebRemote{
				Emails:       emails,
				DTLastChange: r.friendsMostRecentChangeDT,
			}

			go func() {
				if err := r.web.PostRemote(&remote, u); err != nil {
					//fmt.Println(err)
					//os.Exit(0)
				}
			}()
			r.friendsLastPushDT = r.friendsMostRecentChangeDT
		}
	}
}

func (r *DBListRepo) startSync(ctx context.Context, replayChan chan []EventLog, reorderAndReplayChan chan []EventLog) error {
	syncTriggerChan := make(chan struct{})
	pushTriggerTimer := time.NewTimer(time.Second * 0)
	// Drain the initial push timer, we want to wait for initial user input
	// We do however schedule an initial iteration of a gather to ensure all local state (including any files manually
	// dropped in to the root directory, etc) are flushed
	<-pushTriggerTimer.C

	webPingTicker := time.NewTicker(webPingInterval)
	webRefreshTicker := time.NewTicker(webRefreshInterval)
	// We set the interval to 0 because we want the initial connection establish attempt to occur ASAP
	webRefreshTicker.Reset(0)

	websocketPushEvents := make(chan websocketMessage)

	scheduleSync := func() {
		select {
		case syncTriggerChan <- struct{}{}:
		default:
		}
	}

	// Run an initial load from the local walfile
	localWal, err := r.pull(ctx, func() <-chan WalFile {
		ch := make(chan WalFile)
		go func() {
			defer close(ch)
			ch <- r.LocalWalFile
		}()
		return ch
	}())
	if err != nil {
		return err
	}
	replayChan <- localWal

	// Main sync event loop
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				//var c chan []EventLog
				var el []EventLog
				var err error
				syncWalfiles := r.walFileGen(walFileCategorySync)
				if el, err = r.pull(ctx, syncWalfiles); err != nil {
					log.Fatal(err)
				}
				//c = replayChan
				//c = reorderAndReplayChan

				// Rather than relying on a ticker (which will trigger the next cycle if processing time is >= the interval)
				// we set a wait interval from the end of processing. This prevents a vicious circle which could leave the
				// program with it's CPU constantly tied up, which leads to performance degradation.
				// Instead, at the end of the processing cycle, we schedule a wait period after which the next event is put
				// onto the syncTriggerChan
				// we block on replayChan publish so it only schedules again after a Replay begins
				if len(el) > 0 {
					replayChan <- el
				}
				time.Sleep(time.Second * time.Duration(pullIntervalSeconds))
				scheduleSync()
			}
		}
	}()

	// Prioritise async web start-up to minimise wait time before websocket instantiation
	// Create a loop responsible for periodic refreshing of web connections and web walfiles.
	// TODO only start this goroutine if there is actually a configured web
	go func() {
		expBackoffInterval := time.Second * 1
		var waitInterval time.Duration
		var webCtx context.Context
		var webCancel context.CancelFunc
		for {
			select {
			case <-webPingTicker.C:
				// is !isActive, we've already entered the exponential retry backoff below
				if r.web.isActive {
					if _, err := r.web.ping(); err != nil {
						r.web.isActive = false
						webRefreshTicker.Reset(0)
						continue
					}
				}
			case m := <-websocketPushEvents:
				if r.web.isActive {
					r.web.pushWebsocket(m)
				}
			case <-webRefreshTicker.C:
				if webCancel != nil {
					webCancel()
				}
				// Close off old websocket connection
				// Nil check because initial instantiation also occurs async in this loop (previous it was sync on startup)
				if r.web.wsConn != nil {
					r.web.wsConn.Close(websocket.StatusNormalClosure, "")
				}
				// Start new one
				err := r.registerWeb()
				if err != nil {
					r.web.isActive = false
					switch err.(type) {
					case authFailureError:
						return // authFailureError signifies incorrect login details, disable web and run local only mode
					default:
						waitInterval = expBackoffInterval
						if expBackoffInterval < webRefreshInterval {
							expBackoffInterval *= 2
						}
					}
				} else {
					r.web.isActive = true
					expBackoffInterval = time.Second * 1
					waitInterval = webRefreshInterval
				}
				// Trigger web walfile sync (mostly relevant on initial start)
				scheduleSync()

				webCtx, webCancel = context.WithCancel(ctx)
				if r.web.isActive {
					go func() {
						for {
							wsEl, err := r.consumeWebsocket(webCtx)
							if err != nil {
								// webCancel() triggers error which we need to handle and return
								// to avoid haywire goroutines with infinite loops and CPU destruction
								return
							}
							// Rather than clogging up numerous goroutines waiting to publish
							// single item event logs to replayChan, we attempt to publish to it,
							// but if there's already a wal pending, we fail back and write to
							// an aggregated log, which will then be attempted again on the next
							// incoming event
							// TODO if we get a failed publish and then no more incoming websocket
							// events, the aggregated log will never be published to replayChan
							// use a secondary ticker that will wait a given period and flush if
							// needed????
							if len(wsEl) > 0 {
								go func() {
									replayChan <- wsEl
								}()
							}
						}
					}()
				}

				webRefreshTicker.Reset(waitInterval)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Create a loop to deal with any collaborator cursor move events
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				e := <-r.localCursorMoveChan
				func() {
					r.webWalFileMut.RLock()
					defer r.webWalFileMut.RUnlock()
					for _, wf := range r.webWalFiles {
						go func(uuid string) {
							websocketPushEvents <- websocketMessage{
								Action:       "position",
								UUID:         uuid,
								Key:          e.listItemKey,
								UnixNanoTime: e.unixNanoTime,
							}
						}(wf.GetUUID())
					}
				}()
			}
		}
	}()

	// Push to all WalFiles
	go func() {
		el := []EventLog{}
		for {
			// The events chan contains single events. We want to aggregate them between intervals
			// and then emit them in batches, for great efficiency gains.
			select {
			case e := <-r.eventsChan:
				// Write in real time to the websocket, if present
				if r.web.isActive {
					func() {
						r.webWalFileMut.RLock()
						defer r.webWalFileMut.RUnlock()
						for _, wf := range r.webWalFiles {
							if matchedEventLog := r.getMatchedWal([]EventLog{e}, wf); len(matchedEventLog) > 0 {
								// There are only single events, so get the zero index
								b, _ := buildByteWal(matchedEventLog)
								b64Wal := base64.StdEncoding.EncodeToString(b.Bytes())
								go func(uuid string) {
									websocketPushEvents <- websocketMessage{
										Action: "wal",
										UUID:   uuid,
										Wal:    b64Wal,
									}
								}(wf.GetUUID())
							}
						}
					}()

					// Emit any remote updates if web active and local changes have occurred
					r.emitRemoteUpdate()
				}
				// Add to an ephemeral log
				el = append(el, e)
				// Trigger an aggregated push (if not already pending)
				pushTriggerTimer.Reset(pushWaitDuration)
			case <-pushTriggerTimer.C:
				// On ticks, Flush what we've aggregated to all walfiles, and then reset the
				// ephemeral log. If empty, skip.
				r.flushPartialWals(ctx, el, false)
				el = []EventLog{}
			case <-ctx.Done():
				// TODO create a separate timeout here? This is the only case where we don't want the parent
				// context cancellation to cancel the inflight flush
				// TODO ensure completion of this operation before completing?? maybe bring back in the stop chan
				r.flushPartialWals(context.Background(), el, true)
				go func() {
					r.stopChan <- struct{}{}
				}()
				return
			}
		}
	}()

	return nil
}

func (r *DBListRepo) finish(purge bool) error {
	// When we pull wals from remotes, we merge into our in-mem logs, but will only flush to local walfile
	// on gather. To ensure we store all logs locally, for now, we can just push the entire in-mem log to
	// the local walfile. We can remove any other files to avoid overuse of local storage.
	// TODO this can definitely be optimised (e.g. only flush a partial log of unpersisted changes, or perhaps
	// track if any new wals have been pulled, etc)
	if !purge {
		ctx := context.Background()
		checksum, err := r.push(ctx, r.LocalWalFile, r.log, nil)
		if err != nil {
			return err
		}
		localFiles, _ := r.LocalWalFile.GetMatchingWals(ctx, fmt.Sprintf(path.Join(r.LocalWalFile.GetRoot(), walFilePattern), "*"))
		if len(localFiles) > 0 {
			filesToDelete := make([]string, len(localFiles)-1)
			for _, f := range localFiles {
				if f != checksum {
					filesToDelete = append(filesToDelete, f)
				}
			}
			r.LocalWalFile.RemoveWals(ctx, filesToDelete)
		}
	} else {
		<-r.stopChan
		// If purge is set, we delete everything in the local walfile. This is used primarily in the wasm browser app on logout
		r.LocalWalFile.Purge()
	}

	if r.web.wsConn != nil {
		r.web.wsConn.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}

// BuildWalFromPlainText accepts an io.Reader with line separated plain text, and generates a wal db file
// which is dumped in fzn root directory.
func BuildWalFromPlainText(ctx context.Context, wf WalFile, r io.Reader, isHidden bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Split(bufio.ScanLines)
	el := []EventLog{}

	// any random UUID is fine
	uuid := generateUUID()
	var lamportTimestamp int64
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}

		key := strconv.Itoa(int(uuid)) + ":" + strconv.Itoa(int(lamportTimestamp))
		el = append(el, EventLog{
			UUID:             uuid,
			EventType:        AddEvent,
			LamportTimestamp: lamportTimestamp,
			ListItemKey:      key,
			Line:             line,
		})
		lamportTimestamp++

		if isHidden {
			el = append(el, EventLog{
				UUID:             uuid,
				EventType:        HideEvent,
				LamportTimestamp: lamportTimestamp,
				ListItemKey:      key,
			})
			lamportTimestamp++
		}
	}

	b, _ := buildByteWal(el)
	wf.Flush(ctx, b, fmt.Sprintf("%d", uuid))

	return nil
}
