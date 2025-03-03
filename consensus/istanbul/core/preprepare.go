// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"math/big"
	"time"

	"github.com/Venachain/Venachain/rlp"
	"github.com/pkg/errors"

	"github.com/Venachain/Venachain/common"
	"github.com/Venachain/Venachain/consensus"
	"github.com/Venachain/Venachain/consensus/istanbul"
)

func (c *core) sendPreprepare(request *istanbul.Request) {
	logger := c.logger.New("state", c.state)
	// If I'm the proposer and I have the same sequence with the proposal
	if c.current.Sequence().Cmp(request.Proposal.Number()) == 0 && c.IsProposer() {
		curView := c.currentView()
		preprepare := &istanbul.Preprepare{
			View:           curView,
			Proposal:       request.Proposal,
			LockedRound:    big.NewInt(0),
			LockedHash:     common.Hash{},
			LockedPrepares: newMessageSet(c.current.Prepares.valSet),
		}

		if c.current.IsHashLocked() {
			preprepare.LockedRound = c.current.lockedRound
			preprepare.LockedHash = c.current.lockedHash
			preprepare.LockedPrepares = c.current.lockedPrepares
		}
		logger.Debug("sendPreprepare", "proposal", preprepare.Proposal.Hash(), "view", preprepare.View.String())

		encodedPreprepare, err := Encode(preprepare)
		if err != nil {
			logger.Error("Failed to encode", "view", curView, "err", err)
			return
		}

		c.broadcast(&message{
			Code: msgPreprepare,
			Msg:  encodedPreprepare,
		})
	}
}

func (c *core) handlePreprepare(msg *message, src istanbul.Validator) error {
	logger := c.logger.New("from", src, "state", c.state)
	// Decode PRE-PREPARE
	var preprepare *istanbul.Preprepare
	err := msg.Decode(&preprepare)
	if err != nil {
		return errFailedDecodePreprepare
	}

	// Ensure we have the same view with the PRE-PREPARE message
	// If it is old message, see if we need to broadcast COMMIT
	if err := c.checkMessage(msgPreprepare, preprepare.View); err != nil {
		if err == errOldMessage {
			// Get validator set for the given proposal
			valSet := c.backend.ParentValidators(preprepare.Proposal).Copy()
			previousProposer := c.backend.GetProposer(preprepare.Proposal.Number().Uint64() - 1)
			valSet.CalcProposer(previousProposer, preprepare.View.Round.Uint64())
			// Broadcast COMMIT if it is an existing block
			// 1. The proposer needs to be a proposer matches the given (Sequence + Round)
			// 2. The given block must exist
			if valSet.IsProposer(src.Address()) && c.backend.HasPropsal(preprepare.Proposal.Hash(), preprepare.Proposal.Number()) {
				c.sendCommitForOldBlock(preprepare.View, preprepare.Proposal.Hash())
				return nil
			}
		}
		return err
	}

	// Check if the message comes from current proposer
	if !c.valSet.IsProposer(src.Address()) {
		logger.Warn("Ignore preprepare messages from non-proposer")
		return errNotFromProposer
	}

	// Verify the proposal we received
	// c.roundChangeTimer.Reset(time.Millisecond * time.Duration(c.config.RequestTimeout))
	if duration, err := c.backend.Verify(preprepare.Proposal, c.valSet.IsProposer(c.address)); err != nil {
		logger.Warn("Failed to verify proposal", "err", err, "duration", duration)
		// if it's a future block, we will handle it again after the duration
		if err == consensus.ErrFutureBlock {
			c.stopFuturePreprepareTimer()
			c.futurePreprepareTimer = time.AfterFunc(duration, func() {
				c.sendEvent(backlogEvent{
					src: src,
					msg: msg,
				})
			})
		} else {
			c.sendNextRoundChange()
		}
		return err
	}

	// Here is about to accept the PRE-PREPARE
	if c.state == StateAcceptRequest {
		// Send ROUND CHANGE if the locked proposal and the received proposal are different
		if c.current.IsHashLocked() {
			if preprepare.Proposal.Hash() == c.current.GetLockedHash() {
				// Broadcast COMMIT and enters Prepared state directly
				c.acceptPreprepare(preprepare)
				c.setState(StatePrepared)
				c.sendCommit()
			} else {
				if preprepare.LockedRound.Cmp(c.current.lockedRound) >= 0 && !common.EmptyHash(preprepare.LockedHash) && preprepare.LockedHash == preprepare.Proposal.Hash() {
					logger.Debug("handle POLPreprepare")
					return c.handlePOLPreprepare(src, preprepare)
				}

				// Send round change
				c.sendNextRoundChange()
			}
		} else {
			// Either
			//   1. the locked proposal and the received proposal match
			//   2. we have no locked proposal
			c.logger.Debug("received preprepare,", "preprepar", preprepare)
			c.acceptPreprepare(preprepare)
			c.setState(StatePreprepared)
			c.sendPrepare()
		}
	}

	return nil
}

func (c *core) handlePOLPreprepare(src istanbul.Validator, preprepare *istanbul.Preprepare) error {
	logger := c.logger.New("from", src, "state", c.state)
	logger.Info("handlePOLPreprepare")

	_, r, err := rlp.EncodeToReader(preprepare.LockedPrepares)
	if nil != err {
		logger.Error("Failed to EncodeToReader", "LockedPrepares", preprepare.LockedPrepares, "err", err)
		return err
	}

	prepares := newMessageSet(c.valSet)
	err = rlp.Decode(r, prepares)
	if nil != err {
		logger.Error("Failed to decode", "LockedPrepares", preprepare.LockedPrepares, "err", err)
		return err
	}

	// check if the vote is invalid
	valPrepares := prepares.Values()
	for _, m := range valPrepares {
		err := m.Validate(c.validateFn)
		if nil != err {
			logger.Error("Failed to validate msg", "err", err)
			return err
		}
	}

	if prepares.Size() < c.valSet.Size()-c.valSet.F() {
		logger.Error("POLPrePrepare is invalid, lockedprepares vote -2/3")
		return errors.New("POLPrePrepare is invalid, lockedprepares vote -2/3")
	}

	c.current.UnlockHash()

	c.acceptPreprepare(preprepare)
	c.setState(StatePreprepared)
	c.sendPrepare()

	return nil
}

func (c *core) acceptPreprepare(preprepare *istanbul.Preprepare) {
	c.consensusTimestamp = time.Now()
	c.current.SetPreprepare(preprepare)
}
