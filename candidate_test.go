package consultant_test

import (
	"fmt"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/testutil"
	"github.com/myENA/consultant"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"sync"
	"testing"
)

const (
	candidateLockKey = "consultant/tests/candidate-lock"
	candidateLockTTL = "5s"
)

type CandidateTestSuite struct {
	suite.Suite

	// these values are cyclical, and should be re-defined per test method
	server *testutil.TestServer
	client *consultant.Client
}

func TestCandidate(t *testing.T) {
	suite.Run(t, &CandidateTestSuite{})
}

// SetupTest is called before each method is run.
func (cs *CandidateTestSuite) SetupTest() {
	cs.server, cs.client = makeServerAndClient(cs.T(), nil)
}

// TearDownTest is called after each method has been run.
func (cs *CandidateTestSuite) TearDownTest() {
	if nil != cs.client {
		cs.client = nil
	}
	if nil != cs.server {
		// TODO: Stop seems to return an error when the process is killed...
		cs.server.Stop()
		cs.server = nil
	}
}

func (cs *CandidateTestSuite) TearDownSuite() {
	cs.TearDownTest()
}

func (cs *CandidateTestSuite) makeCandidate(num int) *consultant.Candidate {
	candidate, err := consultant.NewCandidate(cs.client, fmt.Sprintf("test-%d", num), candidateLockKey, candidateLockTTL)
	if nil != err {
		cs.T().Fatalf("err: %v", err)
	}

	return candidate
}

func (cs *CandidateTestSuite) TestSimpleElectionCycle() {
	var candidate1, candidate2, candidate3 *consultant.Candidate
	var leader *api.SessionEntry
	var err error

	wg := new(sync.WaitGroup)

	wg.Add(3)

	go func() {
		candidate1 = cs.makeCandidate(1)
		candidate1.Wait()
		wg.Done()
	}()
	go func() {
		candidate2 = cs.makeCandidate(2)
		candidate2.Wait()
		wg.Done()
	}()
	go func() {
		candidate3 = cs.makeCandidate(3)
		candidate3.Wait()
		wg.Done()
	}()

	wg.Wait()

	leader, err = candidate1.Leader()
	require.Nil(cs.T(), err, fmt.Sprintf("Unable to locate leader session entry: %v", err))

	require.True(
		cs.T(),
		leader.ID == candidate1.SessionID() ||
			leader.ID == candidate2.SessionID() ||
			leader.ID == candidate3.SessionID(),
		fmt.Sprintf(
			"Expected one of \"%+v\", saw \"%s\"",
			[]string{candidate1.SessionID(), candidate2.SessionID(), candidate3.SessionID()},
			leader.ID))

	wg.Add(3)

	go func() {
		candidate1.Resign()
		wg.Done()
	}()
	go func() {
		candidate2.Resign()
		wg.Done()
	}()
	go func() {
		candidate3.Resign()
		wg.Done()
	}()

	wg.Wait()

	leader, err = candidate1.Leader()
	require.NotNil(cs.T(), err, "Expected empty key error, got nil")
	require.Nil(cs.T(), leader, fmt.Sprintf("Expected nil leader, got %v", leader))
}
