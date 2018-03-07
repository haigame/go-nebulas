// Copyright (C) 2018 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package mvccdb

import (
	"bytes"
	"errors"
	"sync"

	"github.com/nebulasio/go-nebulas/storage"
	"github.com/nebulasio/go-nebulas/util/logging"
	"github.com/sirupsen/logrus"

	"github.com/nebulasio/go-nebulas/util/byteutils"
)

var (
	ErrStagingTableKeyConfliction = errors.New("staging table key confliction")
)

type stagingValuesMap map[string]*VersionizedValueItem
type stagingValuesMapMap map[interface{}]stagingValuesMap

// VersionizedValueItem a struct for key/value pair, with version, dirty, deleted flags.
type VersionizedValueItem struct {
	tid     interface{}
	key     []byte
	val     []byte
	old     int
	new     int
	deleted bool
	dirty   bool
}

// StagingTable a struct to store all staging changed key/value pairs.
// There are two map to store the key/value pairs. One are stored associated with tid,
// the other is `finalVersionizedValue`, record the `ready to commit` key/value pairs.
type StagingTable struct {
	storage                    storage.Storage
	allVersionizedValues       map[interface{}]stagingValuesMap
	finalVersionizedValue      stagingValuesMap
	tidMutex                   sync.Mutex
	finalVersionizedValueMutex sync.Mutex
}

// NewStagingTable return new instance of StagingTable.
func NewStagingTable(storage storage.Storage) *StagingTable {
	return &StagingTable{
		storage:               storage,
		allVersionizedValues:  make(map[interface{}]stagingValuesMap),
		finalVersionizedValue: make(stagingValuesMap),
	}
}

// Get return value by tid and key. If tid+key does not exist, copy and incr version from `finalVersionaizeValues` to record previous version.
func (tbl *StagingTable) Get(tid interface{}, key []byte) (*VersionizedValueItem, error) {
	return tbl.getVersionizedValueForUpdate(tid, key)
}

// Put put the tid/key/val pair. If tid+key does not exist, copy and incr version from `finalVersionaizeValues` to record previous version.
func (tbl *StagingTable) Put(tid interface{}, key []byte, val []byte) (*VersionizedValueItem, error) {
	value, err := tbl.getVersionizedValueForUpdate(tid, key)
	if err != nil {
		return nil, err
	}

	value.dirty = value.dirty || bytes.Compare(value.val, val) != 0
	value.val = val
	return value, nil
}

// Set set the tid/key/val pair. If tid+key does not exist, copy and incr version from `finalVersionaizeValues` to record previous version.
func (tbl *StagingTable) Set(tid interface{}, key []byte, val []byte, deleted, dirty bool) (*VersionizedValueItem, error) {
	value, err := tbl.getVersionizedValueForUpdate(tid, key)
	if err != nil {
		return nil, err
	}

	value.val = val
	value.deleted = deleted
	value.dirty = dirty
	return value, nil
}

// Del del the tid/key pair. If tid+key does not exist, copy and incr version from `finalVersionaizeValues` to record previous version.
func (tbl *StagingTable) Del(tid interface{}, key []byte) (*VersionizedValueItem, error) {
	value, err := tbl.getVersionizedValueForUpdate(tid, key)
	if err != nil {
		return nil, err
	}

	value.deleted = true
	value.dirty = true
	return value, nil
}

// Purge purge key/value pairs of tid.
func (tbl *StagingTable) Purge(tid interface{}) {
	if tid == nil {
		return
	}

	tbl.tidMutex.Lock()
	defer tbl.tidMutex.Unlock()

	delete(tbl.allVersionizedValues, tid)
}

// MergeToFinal merge key/value pair of tid to `finalVersionizedValues` which the version of value are the same.
func (tbl *StagingTable) MergeToFinal(tid interface{}) ([]interface{}, error) {
	tbl.finalVersionizedValueMutex.Lock()
	defer tbl.finalVersionizedValueMutex.Unlock()

	dependentTids := make(map[interface{}]bool)
	conflictKeys := make(map[string]interface{})

	// 1. check version.
	finalValues := tbl.finalVersionizedValue
	tidValues := tbl.getVersionizedValuesOfTid(tid)

	for keyStr, tidValueItem := range tidValues {
		finalValueItem := finalValues[keyStr]

		// record conflict.
		if tidValueItem.old != finalValueItem.new {
			conflictKeys[keyStr] = finalValueItem.tid
			continue
		}

		// skip default value loaded from storage.
		if finalValueItem.isDefault() {
			continue
		}

		// record dependentTids.
		dependentTids[finalValueItem.tid] = true
	}

	if len(conflictKeys) > 0 {
		logging.VLog().WithFields(logrus.Fields{
			"tid":          tid,
			"conflictKeys": conflictKeys,
		}).Debug("Check failed.")
		return nil, ErrStagingTableKeyConfliction
	}

	// 2. merge to final.
	for keyStr, tidValueItem := range tidValues {
		// ignore dirty.
		if !tidValueItem.dirty {
			continue
		}

		// merge.
		finalValues[keyStr] = tidValueItem.CloneForFinal()
	}

	tids := make([]interface{}, 0, len(dependentTids))
	for key := range dependentTids {
		tids = append(tids, key)
	}

	return tids, nil
}

// LockFinalVersionValue lock to read/write `finalVersionizedValue`.
func (tbl *StagingTable) LockFinalVersionValue() {
	tbl.finalVersionizedValueMutex.Lock()
}

// UnlockFinalVersionValue unlock.
func (tbl *StagingTable) UnlockFinalVersionValue() {
	tbl.finalVersionizedValueMutex.Unlock()
}

func (tbl *StagingTable) getVersionizedValueForUpdate(tid interface{}, key []byte) (*VersionizedValueItem, error) {
	keyStr := byteutils.Hex(key)
	tidValues := tbl.getVersionizedValuesOfTid(tid)

	value := tidValues[keyStr]
	if value == nil {
		var err error
		value, err = tbl.getAndIncrValueFromFinalVersionValue(tid, keyStr, key)
		if err != nil {
			return nil, err
		}

		tidValues[keyStr] = value
	}

	return value, nil
}

func (tbl *StagingTable) getVersionizedValuesOfTid(tid interface{}) stagingValuesMap {
	if tid == nil {
		return tbl.finalVersionizedValue
	}

	tbl.tidMutex.Lock()
	defer tbl.tidMutex.Unlock()

	tidValues := tbl.allVersionizedValues[tid]
	if tidValues == nil {
		tidValues = make(stagingValuesMap)
		tbl.allVersionizedValues[tid] = tidValues
	}
	return tidValues
}

func (tbl *StagingTable) getAndIncrValueFromFinalVersionValue(tid interface{}, keyStr string, key []byte) (*VersionizedValueItem, error) {
	tbl.finalVersionizedValueMutex.Lock()
	defer tbl.finalVersionizedValueMutex.Unlock()

	latestValue := tbl.finalVersionizedValue[keyStr]
	if latestValue == nil {

		// get from storage.
		val, err := tbl.storage.Get(key)
		if err != nil && err != storage.ErrKeyNotFound {
			return nil, err
		}

		latestValue = NewDefaultVersionizedValueItem(key, val)
		tbl.finalVersionizedValue[keyStr] = latestValue
	}

	// incr version.
	value := IncrVersionizedValueItem(tid, latestValue)
	return value, nil
}

func (value *VersionizedValueItem) isDefault() bool {
	return value.tid == nil && value.new == 0 && value.old == 0
}

// NewDefaultVersionizedValueItem return new instance of VersionizedValueItem, old/new version are 0, dirty is false.
func NewDefaultVersionizedValueItem(key []byte, val []byte) *VersionizedValueItem {
	return &VersionizedValueItem{
		tid:     nil,
		key:     key,
		val:     val,
		old:     0,
		new:     0,
		deleted: false,
		dirty:   false,
	}
}

// IncrVersionizedValueItem copy and return the version increased VersionizedValueItem.
func IncrVersionizedValueItem(tid interface{}, oldValue *VersionizedValueItem) *VersionizedValueItem {
	return &VersionizedValueItem{
		tid:     tid,
		key:     oldValue.key,
		val:     oldValue.val,
		old:     oldValue.new,
		new:     oldValue.new + 1,
		deleted: oldValue.deleted,
		dirty:   false,
	}
}

// CloneForFinal shadow copy of `VersionizedValueItem` with dirty is true.
func (value *VersionizedValueItem) CloneForFinal() *VersionizedValueItem {
	return &VersionizedValueItem{
		tid:     value.tid,
		key:     value.key,
		val:     value.val,
		old:     value.old,
		new:     value.new,
		deleted: value.deleted,
		dirty:   true,
	}
}