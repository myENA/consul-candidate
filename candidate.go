package consultant

import (
	"errors"
	"fmt"
	"github.com/hashicorp/consul/api"
	"github.com/renstrom/shortuuid"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	validCandidateIDRegex = `[a-zA-Z0-9:._-]+` // only allow certain characters in an ID
	CandidateMaxWait      = 10                 // specifies the maximum wait after failed lock checks
)

var (
	validCandidateIDTest = regexp.MustCompile(validCandidateIDRegex)
	candidateIDErrMsg    = fmt.Sprintf("Candidate ID must obey \"%s\"", validCandidateIDRegex)
)

type Candidate struct {
	client *Client

	mu sync.Mutex

	id           string
	logSlug      string
	logSlugSlice []interface{}

	kv           *api.KVPair
	sessionTTL   time.Duration
	sessionID    string
	sessionEntry *api.SessionEntry

	wait    *sync.WaitGroup
	leader  bool
	closing bool

	update map[string]chan bool
}

// NewCandidate creates a new Candidate
//
// - "client" must be a valid api client
//
// - "id" should be an implementation-relevant unique identifier
//
// - "key" must be the full path to a KV, it will be created if it doesn't already exist
//
// - "ttl" is the duration to set on the kv session ttl, will default to 30s if not specified
func NewCandidate(client *Client, id, key, ttl string) (*Candidate, error) {
	var err error
	var ttlSeconds float64

	id = strings.TrimSpace(id)

	if !validCandidateIDTest.MatchString(id) {
		return nil, errors.New(candidateIDErrMsg)
	}

	c := &Candidate{
		client: client,
		id:     id,
		wait:   new(sync.WaitGroup),
		update: make(map[string]chan bool),
	}

	// create some slugs for log output
	c.logSlug = fmt.Sprintf("[candidate-%s] ", id)
	c.logSlugSlice = []interface{}{c.logSlug}

	// begin session entry construction
	c.sessionEntry = &api.SessionEntry{
		Name:     fmt.Sprintf("leader-%s-%s-%s", id, c.client.MyNode(), shortuuid.New()),
		Behavior: api.SessionBehaviorDelete,
	}

	if debug {
		c.logPrintf("Session name \"%s\"", c.sessionEntry.Name)
	}

	// if ttl empty, default to 30 seconds
	if "" == ttl {
		ttl = "30s"
	}

	// validate ttl
	c.sessionTTL, err = time.ParseDuration(ttl)
	if nil != err {
		return nil, fmt.Errorf("unable to parse provided TTL: %v", err)
	}

	// stay within the limits...
	ttlSeconds = c.sessionTTL.Seconds()
	if 10 > ttlSeconds {
		c.sessionEntry.TTL = "10s"
	} else if 86400 < ttlSeconds {
		c.sessionEntry.TTL = "86400s"
	} else {
		c.sessionEntry.TTL = ttl
	}

	if debug {
		c.logPrintf("Setting TTL to \"%s\"", c.sessionEntry.TTL)
	}

	// create new session
	c.sessionID, _, err = c.client.Session().Create(c.sessionEntry, nil)
	if nil != err {
		return nil, err
	}

	if debug {
		c.logPrintf("Session created with id \"%s\"", c.sessionID)
	}

	// store session id
	c.sessionEntry.ID = c.sessionID

	c.kv = &api.KVPair{
		Key:     key,
		Session: c.sessionID,
	}

	go c.sessionKeepAlive()
	go c.lockRunner()

	return c, nil
}

// ID returns the unique identifier given at construct
func (c *Candidate) ID() string {
	c.mu.Lock()
	id := c.id
	c.mu.Unlock()
	return id
}

// SessionID is the name of this candidate's session
func (c *Candidate) SessionID() string {
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	return sid
}

// SessionTTL returns the parsed TTL
func (c *Candidate) SessionTTL() time.Duration {
	c.mu.Lock()
	ttl := c.sessionTTL
	c.mu.Unlock()
	return ttl
}

// Elected will return true if this candidate's session is "locking" the kv
func (c *Candidate) Elected() bool {
	c.mu.Lock()
	el := c.leader
	c.mu.Unlock()
	return el
}

// Resign makes this candidate defunct.
func (c *Candidate) Resign() {
	c.mu.Lock()
	if !c.leader && c.closing {
		if debug {
			c.logPrint("Resign called while not in election pool...")
		}
		c.mu.Unlock()
		return
	}

	c.leader = false
	c.closing = true
	c.mu.Unlock()

	if debug {
		c.logPrint("Resigning candidacy ... waiting for routines to exit ...")
	}

	c.wait.Wait()

	c.logPrint("Candidacy resigned.  We're no longer in the running")
}

// LeaderService will attempt to locate the leader's session entry in your local agent's datacenter
func (c *Candidate) LeaderService() (*api.SessionEntry, error) {
	return c.ForeignLeaderService("")
}

// Return the leader, assuming its ID can be interpreted as an IP address
func (c *Candidate) LeaderIP() (net.IP, error) {
	return c.ForeignLeaderIP("")

}

// Return the leader of a foreign datacenter, assuming its ID can be interpreted as an IP address
func (c *Candidate) ForeignLeaderIP(dc string) (net.IP, error) {
	leaderSession, err := c.ForeignLeaderService(dc)
	if nil != err {
		return nil, fmt.Errorf("leaderAddress() Error getting leader address: %s", err)
	}

	// parse session name
	parts, err := ParseCandidateSessionName(leaderSession.Name)
	if nil != err {
		return nil, fmt.Errorf("leaderAddress() Unable to parse leader session name: %s", err)
	}

	// attempt to validate value
	ip := net.ParseIP(parts.ID)
	if nil == ip {
		return nil, fmt.Errorf("leaderAddress() Unable to parse IP address from \"%s\"", parts.ID)
	}

	return ip, nil
}

// ForeignLeaderService will attempt to locate the leader's session entry in a datacenter of your choosing
func (c *Candidate) ForeignLeaderService(dc string) (*api.SessionEntry, error) {
	var kv *api.KVPair
	var se *api.SessionEntry
	var err error

	qo := &api.QueryOptions{}

	if "" != dc {
		qo.Datacenter = dc
	}

	kv, _, err = c.client.KV().Get(c.kv.Key, qo)
	if nil != err {
		return nil, err
	}

	if nil == kv {
		return nil, fmt.Errorf("kv \"%s\" not found in datacenter \"%s\"", c.kv.Key, dc)
	}

	if "" != kv.Session {
		se, _, err = c.client.Session().Info(kv.Session, qo)
		if nil != se {
			return se, nil
		}
	}

	return nil, fmt.Errorf("kv \"%s\" has no session in datacenter \"%s\"", c.kv.Key, dc)
}

// Wait will block until a leader has been elected, regardless of candidate.
func (c *Candidate) Wait() {
	var err error

	for {
		// attempt to locate current leader
		if _, err = c.LeaderService(); nil == err {
			break
		}

		// if session empty, assume no leader elected yet and try again
		time.Sleep(time.Second * 1)
	}
}

// Register returns a channel for updates in leader status -
// only one message per candidate instance will be sent
func (c *Candidate) RegisterUpdate(id string) (string, chan bool) {
	c.mu.Lock()
	if id == "" {
		id = randToken()
	}
	cup, ok := c.update[id]
	if !ok {
		cup = make(chan bool, 1)
		c.update[id] = cup
	}
	c.mu.Unlock()
	return id, cup
}

func (c *Candidate) DeregisterUpdate(id string) {
	c.mu.Lock()
	_, ok := c.update[id]
	if ok {
		delete(c.update, id)
	}
	c.mu.Unlock()
}

// DeregisterUpdates will empty out the map of update channels
func (c *Candidate) DeregisterUpdates() {
	c.mu.Lock()
	c.update = make(map[string]chan bool)
	c.mu.Unlock()
}

// updateLeader is a thread safe update of leader status
func (c *Candidate) updateLeader(v bool) {
	c.mu.Lock()

	// Send out updates if leader status changed
	if v != c.leader {
		for _, cup := range c.update {
			cup <- v
		}
	}

	// Update the leader flag
	c.leader = v

	c.mu.Unlock()
}

func randToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// lockRunner runs the lock
func (c *Candidate) lockRunner() {
	var se *api.SessionEntry // session entry
	var kv *api.KVPair       // retrieved key
	var qm *api.QueryMeta    // metadata
	var err error            // error holder
	var ok bool              // lock status
	var checkWait int        // fail retry

	// build options
	qo := &api.QueryOptions{
		WaitIndex: uint64(0),
		WaitTime:  c.SessionTTL(),
	}

	// increment wait group
	c.wait.Add(1)

	// cleanup on exit
	defer c.wait.Done()

	// main loop
	for {
		// attempt to get the key
		if kv, qm, err = c.client.KV().Get(c.kv.Key, qo); err != nil {
			// log warning
			c.logPrintf("lockRunner() error checking lock: %s", err)
			// increment counter up to maximum value
			if checkWait < CandidateMaxWait {
				checkWait++
			}
			// log sleep
			if debug {
				c.logPrintf("lockRunner() sleeping for %d seconds before retry ...", checkWait)
			}
			// sleep before retry
			time.Sleep(time.Duration(checkWait) * time.Second)

			continue
		}

		// reset wait on success
		checkWait = 0

		// check closing
		if c.closing {
			if debug {
				c.logPrint("lockRunner() exiting")
			}
			return
		}

		// update index
		qo.WaitIndex = qm.LastIndex

		// check kv
		if kv != nil {
			if kv.Session == c.SessionID() {
				// we are the leader
				c.updateLeader(true)
				continue
			}

			// still going ... check session
			if kv.Session != "" {
				// lock (probably) held by someone else...try to find out who it is
				se, _, err = c.client.Session().Info(kv.Session, nil)
				// check error
				if err != nil {
					// failed to get session - log error
					c.logPrintf("lockRunner() error fetching session: %s", err)
					// renew/rebuild session
					c.sessionValidate()
					// wait for next iteration
					continue
				}
				// check returned session entry
				if se == nil {
					// nil session entry - nil kv and attempt to get the lock below
					kv = nil
				}
			} else {
				// missing session - nil kv and attempt to get the lock below
				kv = nil
			}
		}

		// not the leader
		c.updateLeader(false)

		// check for nil key
		if kv == nil {
			if debug {
				c.logPrint("lockRunner() nil lock or empty session detected - attempting to get lock ...")
			}
			// attempt to get the lock and check for error
			if ok, _, err = c.client.KV().Acquire(c.kv, nil); err != nil {
				c.logPrintf("lockRunner() error failed to aquire lock: %s", err)
				// renew/rebuild session
				c.sessionValidate()
				// wait for next iteration
				continue
			}
			// we might have the lock
			if ok && err == nil {
				c.logPrintf("lockRunner() acquired lock with session %s", c.sessionID)
				// yep .. we're the leader
				c.updateLeader(true)
				continue
			}
		}
	}
}

// sessionValidate renews/recreates the session as needed
func (c *Candidate) sessionValidate() {
	var se *api.SessionEntry // session object
	var sid string           // temp session id
	var err error            // error holder

	// attempt to renew session
	if se, _, err = c.client.Session().Renew(c.SessionID(), nil); err != nil || se == nil {
		// check error
		if err != nil {
			// log error
			c.logPrintf("sessionValidate() failed to renew session: %s", err)
			// destroy session
			c.client.Session().Destroy(c.SessionID(), nil)
		}
		// check session
		if se == nil {
			// log error
			c.logPrint("sessionValidate() failed to renew session: not found")
		}
		// recreate the session
		if sid, _, err = c.client.Session().Create(c.sessionEntry, nil); err != nil {
			c.logPrintf("sessionValidate() failed to rebuild session: %s", err)
			return
		}
		// update session and lock pair
		c.mu.Lock()
		c.sessionID = sid
		c.kv.Session = sid
		c.mu.Unlock()
		// log session rebuild
		if debug {
			c.logPrintf("sessionValidate() registered new session %s", sid)
		}
		// all done
		return
	}
	// renew okay
}

// sessionKeepAlive keeps session and ttl check alive
func (c *Candidate) sessionKeepAlive() {
	var sleepDuration time.Duration // sleep duration
	var sleepTicker *time.Ticker    // sleep timer
	var err error                   // error holder

	// increment wait group
	c.wait.Add(1)

	// cleanup on exit
	defer c.wait.Done()

	// ensure sleep always at least one second
	if sleepDuration = c.sessionTTL / 2; sleepDuration < time.Second {
		sleepDuration = time.Second
	}

	// init ticker channel
	sleepTicker = time.NewTicker(sleepDuration)

	// loop every "sleep" seconds
	for range sleepTicker.C {
		// check closing
		if c.closing {
			if debug {
				c.logPrint("sessionKeepAlive() exiting")
			}
			// destroy session
			if _, err = c.client.Session().Destroy(c.sessionID, nil); err != nil {
				c.logPrintf("sessionKeepAlive() failed to destroy session (%s) %s",
					c.sessionID,
					err)
			}
			// stop ticker and exit
			sleepTicker.Stop()
			return
		}
		// renew/rebuild session
		c.sessionValidate()
	}

	// shouldn't ever happen
	if !c.closing {
		c.logPrint("sessionKeepAlive() exiting unexpectedly")
	}
}

func (c *Candidate) logPrintf(format string, v ...interface{}) {
	log.Printf(fmt.Sprintf("%s %s", c.logSlug, format), v...)
}

func (c *Candidate) logPrint(v ...interface{}) {
	log.Print(append(c.logSlugSlice, v...)...)
}

type CandidateSessionParts struct {
	Prefix     string
	ID         string
	NodeName   string
	RandomUUID string
}

// ParseCandidateSessionName is provided so you don't have to parse it yourself :)
func ParseCandidateSessionName(name string) (*CandidateSessionParts, error) {
	// fmt.Sprintf("leader-%s-%s-%s", id, c.client.MyNode(), shortuuid.New()),
	split := strings.Split(name, "-")
	if 4 != len(split) {
		return nil, fmt.Errorf("expected four parts in session name \"%s\", saw only \"%d\"", name, len(split))
	}

	return &CandidateSessionParts{
		Prefix:     split[0],
		ID:         split[1],
		NodeName:   split[2],
		RandomUUID: split[3],
	}, nil
}
