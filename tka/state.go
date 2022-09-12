// Copyright (c) 2022 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tka

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"tailscale.com/types/tkatype"
)

// ErrNoSuchKey is returned if the key referenced by a KeyID does not exist.
var ErrNoSuchKey = errors.New("key not found")

// State describes Tailnet Key Authority state at an instant in time.
//
// State is mutated by applying Authority Update Messages (AUMs), resulting
// in a new State.
type State struct {
	// LastAUMHash is the blake2s digest of the last-applied AUM.
	// Because AUMs are strictly ordered and form a hash chain, we
	// check the previous AUM hash in an update we are applying
	// is the same as the LastAUMHash.
	LastAUMHash *AUMHash `cbor:"1,keyasint"`

	// DisablementSecrets are KDF-derived values which can be used
	// to turn off the TKA in the event of a consensus-breaking bug.
	//
	// TODO(tom): This is an alpha feature, remove this mechanism once
	//            we have confidence in our implementation.
	DisablementSecrets [][]byte `cbor:"2,keyasint"`

	// Keys are the public keys currently trusted by the TKA.
	Keys []Key `cbor:"3,keyasint"`

	// BannedNodekeys is a denylist of node-keys. Any signatures over
	// these keys are not considered authorized.
	//
	// Each entry is encoded as the MarshalBinary representation of the
	// underlying key.NodePublic.
	BannedNodekeys [][]byte `cbor:"4,keyasint,omitempty"`
}

// GetKey returns the trusted key with the specified KeyID.
func (s State) GetKey(key tkatype.KeyID) (Key, error) {
	for _, k := range s.Keys {
		if bytes.Equal(k.ID(), key) {
			return k, nil
		}
	}

	return Key{}, ErrNoSuchKey
}

// Clone makes an independent copy of State.
//
// NOTE: There is a difference between a nil slice and an empty
// slice for encoding purposes, so an implementation of Clone()
// must take care to preserve this.
func (s State) Clone() State {
	out := State{}

	if s.LastAUMHash != nil {
		dupe := *s.LastAUMHash
		out.LastAUMHash = &dupe
	}

	if s.DisablementSecrets != nil {
		out.DisablementSecrets = make([][]byte, len(s.DisablementSecrets))
		for i := range s.DisablementSecrets {
			out.DisablementSecrets[i] = make([]byte, len(s.DisablementSecrets[i]))
			copy(out.DisablementSecrets[i], s.DisablementSecrets[i])
		}
	}

	if s.Keys != nil {
		out.Keys = make([]Key, len(s.Keys))
		for i := range s.Keys {
			out.Keys[i] = s.Keys[i].Clone()
		}
	}

	if s.BannedNodekeys != nil {
		out.BannedNodekeys = make([][]byte, len(s.BannedNodekeys))
		for i := range s.BannedNodekeys {
			out.BannedNodekeys[i] = make([]byte, len(s.BannedNodekeys[i]))
			copy(out.BannedNodekeys[i], s.BannedNodekeys[i])
		}
	}
	return out
}

// cloneForUpdate is like Clone, except LastAUMHash is set based
// on the hash of the given update.
func (s State) cloneForUpdate(update *AUM) State {
	out := s.Clone()
	aumHash := update.Hash()
	out.LastAUMHash = &aumHash
	return out
}

const disablementLength = 32

var disablementSalt = []byte("tailscale network-lock disablement salt")

func disablementKDF(secret []byte) []byte {
	// time = 4 (3 recommended, booped to 4 to compensate for less memory)
	// memory = 16 (32 recommended)
	// threads = 4
	// keyLen = 32 (256 bits)
	return argon2.Key(secret, disablementSalt, 4, 16*1024, 4, disablementLength)
}

// checkDisablement returns true for a valid disablement secret.
func (s State) checkDisablement(secret []byte) bool {
	derived := disablementKDF(secret)
	for _, candidate := range s.DisablementSecrets {
		if bytes.Equal(derived, candidate) {
			return true
		}
	}
	return false
}

// parentMatches returns true if an AUM can chain to (be applied)
// to the current state.
//
// Specifically, the rules are:
//   - The last AUM hash must match (transitively, this implies that this
//     update follows the last update message applied to the state machine)
//   - Or, the state machine knows no parent (its brand new).
func (s State) parentMatches(update AUM) bool {
	if s.LastAUMHash == nil {
		return true
	}
	return bytes.Equal(s.LastAUMHash[:], update.PrevAUMHash)
}

// applyVerifiedAUM computes a new state based on the update provided.
//
// The provided update MUST be verified: That is, the AUM must be well-formed
// (as defined by StaticValidate()), and signatures over the AUM must have
// been verified.
func (s State) applyVerifiedAUM(update AUM) (State, error) {
	// Validate that the update message has the right parent.
	if !s.parentMatches(update) {
		return State{}, errors.New("parent AUMHash mismatch")
	}

	switch update.MessageKind {
	case AUMNoOp:
		out := s.cloneForUpdate(&update)
		return out, nil

	case AUMCheckpoint:
		return update.State.cloneForUpdate(&update), nil

	case AUMAddKey:
		if update.Key == nil {
			return State{}, errors.New("no key to add provided")
		}
		if _, err := s.GetKey(update.Key.ID()); err == nil {
			return State{}, errors.New("key already exists")
		}
		out := s.cloneForUpdate(&update)
		out.Keys = append(out.Keys, *update.Key)
		return out, nil

	case AUMUpdateKey:
		k, err := s.GetKey(update.KeyID)
		if err != nil {
			return State{}, err
		}
		if update.Votes != nil {
			k.Votes = *update.Votes
		}
		if update.Meta != nil {
			k.Meta = update.Meta
		}
		if err := k.StaticValidate(); err != nil {
			return State{}, fmt.Errorf("updated key fails validation: %v", err)
		}
		out := s.cloneForUpdate(&update)
		for i := range out.Keys {
			if bytes.Equal(out.Keys[i].ID(), update.KeyID) {
				out.Keys[i] = k
			}
		}
		return out, nil

	case AUMRemoveKey:
		idx := -1
		for i := range s.Keys {
			if bytes.Equal(update.KeyID, s.Keys[i].ID()) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return State{}, ErrNoSuchKey
		}
		out := s.cloneForUpdate(&update)
		out.Keys = append(out.Keys[:idx], out.Keys[idx+1:]...)
		return out, nil

	case AUMAddDenylistNodeKey:
		for i := range s.BannedNodekeys {
			if bytes.Equal(update.NodeKey, s.BannedNodekeys[i]) {
				return State{}, errors.New("entry already exists")
			}
		}
		out := s.cloneForUpdate(&update)
		// Validity of node-key already checked by StaticValidate in aumVerify().
		out.BannedNodekeys = append(out.BannedNodekeys, update.NodeKey)
		return out, nil

	case AUMRemoveDenylistNodeKey:
		idx := -1
		for i := range s.BannedNodekeys {
			if bytes.Equal(update.NodeKey, s.BannedNodekeys[i]) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return State{}, errors.New("no such entry")
		}
		out := s.cloneForUpdate(&update)
		out.BannedNodekeys = append(out.BannedNodekeys[:idx], out.BannedNodekeys[idx+1:]...)
		return out, nil

	default:
		// TODO(tom): Instead of erroring, update lastHash and
		// continue (to preserve future compatibility).
		return State{}, fmt.Errorf("unhandled message: %v", update.MessageKind)
	}
}

// Upper bound on checkpoint elements, chosen arbitrarily. Intended to
// cap out insanely large AUMs.
const (
	maxDisablementSecrets = 32
	maxKeys               = 512
	maxDenylistEntries    = 64
)

// staticValidateCheckpoint validates that the state is well-formed for
// inclusion in a checkpoint AUM.
func (s *State) staticValidateCheckpoint() error {
	if s.LastAUMHash != nil {
		return errors.New("cannot specify a parent AUM")
	}
	if len(s.DisablementSecrets) == 0 {
		return errors.New("at least one disablement secret required")
	}
	if numDS := len(s.DisablementSecrets); numDS > maxDisablementSecrets {
		return fmt.Errorf("too many disablement secrets (%d, max %d)", numDS, maxDisablementSecrets)
	}
	for i, ds := range s.DisablementSecrets {
		if len(ds) != disablementLength {
			return fmt.Errorf("disablement[%d]: invalid length (got %d, want %d)", i, len(ds), disablementLength)
		}
		for j, ds2 := range s.DisablementSecrets {
			if i == j {
				continue
			}
			if bytes.Equal(ds, ds2) {
				return fmt.Errorf("disablement[%d]: duplicates disablement[%d]", i, j)
			}
		}
	}

	if len(s.Keys) == 0 {
		return errors.New("at least one key is required")
	}
	if numKeys := len(s.Keys); numKeys > maxKeys {
		return fmt.Errorf("too many keys (%d, max %d)", numKeys, maxKeys)
	}
	for i, k := range s.Keys {
		if err := k.StaticValidate(); err != nil {
			return fmt.Errorf("key[%d]: %v", i, err)
		}
		for j, k2 := range s.Keys {
			if i == j {
				continue
			}
			if bytes.Equal(k.ID(), k2.ID()) {
				return fmt.Errorf("key[%d]: duplicates key[%d]", i, j)
			}
		}
	}

	if numDenys := len(s.BannedNodekeys); numDenys > maxDenylistEntries {
		return fmt.Errorf("too many node-key denylist entries (%d, max %d)", numDenys, maxDenylistEntries)
	}
	for i, nk := range s.BannedNodekeys {
		for j, nk2 := range s.BannedNodekeys {
			if i == j {
				continue
			}
			if bytes.Equal(nk, nk2) {
				return fmt.Errorf("node-key entry %d duplicates entry %d", i, j)
			}
		}
	}
	return nil
}
