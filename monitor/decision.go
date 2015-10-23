// Copyright (c) 2015 Pagoda Box Inc
//
// This Source Code Form is subject to the terms of the Mozilla Public License, v.
// 2.0. If a copy of the MPL was not distributed with this file, You can obtain one
// at http://mozilla.org/MPL/2.0/.
//

package monitor

import (
	"errors"
	"sync"
	"time"
)

var (
	ClusterUnaviable = errors.New("none of the nodes in the cluster are available")
)

type (
	Decider interface {
		Loop(time.Duration)
		ReCheck() error
	}

	Monitor interface {
		GetRole() (string, error)
		Bounce(Candidate) Candidate
		Ready()
	}

	Candidate interface {
		Monitor
		GetDBRole() (string, error)
		SetDBRole(string) error
		HasSynced() (bool, error)
	}

	Performer interface {
		TransitionToActive(Candidate)
		TransitionToBackupOf(Candidate, Candidate)
		TransitionToSingle(Candidate)
		Stop()
	}

	decider struct {
		sync.Mutex

		me        Candidate
		other     Candidate
		monitor   Monitor
		performer Performer
	}
)

func NewDecider(me Candidate, other Candidate, monitor Monitor, performer Performer) Decider {
	decider := decider{
		me:        me,
		other:     other,
		monitor:   monitor,
		performer: performer,
	}
	for {
		// Really we only have to wait for a quorum, 2 out of 3 will allow everything to be ok.
		// But in certain conditions, this node was a backup that was down, and the current active
		// if offline, we need to wait for all 3 nodes.
		// So really we are going to wait for all 3 nodes to make it simple
		// me is already Ready. no need to call it
		other.Ready()
		monitor.Ready()

		err := decider.ReCheck()
		switch err {
		case ClusterUnaviable: // we try again.
		case nil: // the cluster was successfully rechecked
			return decider
		default: // another kind of error occured
			panic(err)
		}
	}
}

// this is the main loop for monitoring the cluster and making any changes needed to
// reflect changes in remote nodes in the cluster
func (decider decider) Loop(check time.Duration) {
	timer := time.Tick(check)
	for range timer {
		decider.ReCheck()
	}
}

// this is used to move a active node to a backup node
func (decider decider) Demote() {
	decider.Lock()
	defer decider.Unlock()

	decider.me.SetDBRole("backup")
	decider.performer.TransitionToBackupOf(decider.me, decider.other)
}

// this is used to move a backup node to an active node
func (decider decider) Promote() {
	decider.Lock()
	defer decider.Unlock()

	decider.me.SetDBRole("active")
	decider.performer.TransitionToActive(decider.me)
}

// Checks the other node in the cluster, falling back to bouncing the check off of the monitor,
// to see if the states between this node and the remote node match up
func (decider decider) ReCheck() error {
	decider.Lock()
	defer decider.Unlock()

	var otherDBRole string
	var err error
	otherDBRole, err = decider.other.GetDBRole()
	if err != nil {
		otherDBRole, err = decider.monitor.Bounce(decider.other).GetDBRole()
		if err != nil {
			// this node can't talk to the other member of the cluster or the monitor
			// if this node is not in single mode it needs to shut off
			if role, err := decider.me.GetDBRole(); role != "single" || err != nil {
				decider.performer.Stop()
				return ClusterUnaviable
			}
			return nil
		}
	}

	// we need to handle multiple possible states that the remote node is in
	switch otherDBRole {
	case "single":
		fallthrough
	case "active":
		decider.me.SetDBRole("backup")
		decider.performer.TransitionToBackupOf(decider.me, decider.other)
	case "dead":
		DBrole, err := decider.me.GetDBRole()
		if err != nil {
			return err
		}
		if DBrole == "backup" {
			// if this node is not synced up to the previous master, then we must wait for the other node to
			// come online
			hasSynced, err := decider.me.HasSynced()
			if err != nil {
				return err
			}
			if !hasSynced {
				decider.performer.Stop()
				return ClusterUnaviable
			}
		}
		decider.me.SetDBRole("single")
		decider.performer.TransitionToSingle(decider.me)
	case "initialized":
		role, err := decider.me.GetRole()
		if err != nil {
			return err
		}
		switch role {
		case "primary":
			decider.me.SetDBRole("active")
			decider.performer.TransitionToActive(decider.me)
		case "secondary":
			decider.me.SetDBRole("backup")
			decider.performer.TransitionToBackupOf(decider.me, decider.other)
		}
	case "backup":
		decider.me.SetDBRole("active")
		decider.performer.TransitionToActive(decider.me)
	}
	return nil
}
