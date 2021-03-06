package db

import (
	"crypto/rand"
	"errors"

	"bazil.org/bazil/fs/clock"
	"bazil.org/bazil/tokens"
	"github.com/boltdb/bolt"
)

var (
	ErrVolNameInvalid        = errors.New("invalid volume name")
	ErrVolNameNotFound       = errors.New("volume name not found")
	ErrVolNameExist          = errors.New("volume name exists already")
	ErrVolumeIDExist         = errors.New("volume ID exists already")
	ErrVolumeIDNotFound      = errors.New("volume ID not found")
	ErrVolumeEpochWraparound = errors.New("volume epoch wraparound")
)

var (
	bucketVolume        = []byte(tokens.BucketVolume)
	bucketVolName       = []byte(tokens.BucketVolName)
	volumeStateDir      = []byte(tokens.VolumeStateDir)
	volumeStateInode    = []byte(tokens.VolumeStateInode)
	volumeStateSnap     = []byte(tokens.VolumeStateSnap)
	volumeStateStorage  = []byte(tokens.VolumeStateStorage)
	volumeStateEpoch    = []byte(tokens.VolumeStateEpoch)
	volumeStateClock    = []byte(tokens.VolumeStateClock)
	volumeStateConflict = []byte(tokens.VolumeStateConflict)
)

func (tx *Tx) initVolumes() error {
	if _, err := tx.CreateBucketIfNotExists(bucketVolume); err != nil {
		return err
	}
	if _, err := tx.CreateBucketIfNotExists(bucketVolName); err != nil {
		return err
	}
	return nil
}

func (tx *Tx) Volumes() *Volumes {
	p := &Volumes{
		volumes: tx.Bucket(bucketVolume),
		names:   tx.Bucket(bucketVolName),
	}
	return p
}

type Volumes struct {
	volumes *bolt.Bucket
	names   *bolt.Bucket
}

func (b *Volumes) GetByName(name string) (*Volume, error) {
	volID := b.names.Get([]byte(name))
	if volID == nil {
		return nil, ErrVolNameNotFound
	}
	bv := b.volumes.Bucket(volID)
	v := &Volume{
		b:  bv,
		id: volID,
	}
	return v, nil
}

func (b *Volumes) GetByVolumeID(volID *VolumeID) (*Volume, error) {
	bv := b.volumes.Bucket(volID[:])
	if bv == nil {
		return nil, ErrVolumeIDNotFound
	}
	v := &Volume{
		b:  bv,
		id: append([]byte(nil), volID[:]...),
	}
	return v, nil
}

// add a new volume.
//
// If the name exists already, returns ErrVolNameExist.
//
// If the volume ID exists already, returns ErrVolIDExist.
func (b *Volumes) add(name string, volID *VolumeID, storage string, sharingKey *SharingKey) (*Volume, error) {
	if name == "" {
		return nil, ErrVolNameInvalid
	}
	n := []byte(name)
	if v := b.names.Get(n); v != nil {
		return nil, ErrVolNameExist
	}

	bv, err := b.volumes.CreateBucket(volID[:])
	if err == bolt.ErrBucketExists {
		return nil, ErrVolumeIDExist
	}
	if err != nil {
		return nil, err
	}

	if err := b.names.Put(n, volID[:]); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateDir); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateInode); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateSnap); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateStorage); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateClock); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateConflict); err != nil {
		return nil, err
	}
	v := &Volume{
		b:  bv,
		id: volID[:],
	}
	if err := v.Storage().Add("default", storage, sharingKey); err != nil {
		return nil, err
	}
	epoch := clock.Epoch(1)
	if err := v.setEpoch(epoch); err != nil {
		return nil, err
	}
	if _, err := v.Clock().Create(0, "", epoch); err != nil {
		return nil, err
	}
	return v, nil
}

// Create a totally new volume, not yet shared with any peers.
//
// If the name exists already, returns ErrVolNameExist.
func (b *Volumes) Create(name string, storage string, sharingKey *SharingKey) (*Volume, error) {
random:
	id, err := randomVolumeID()
	if err != nil {
		return nil, err
	}
	v, err := b.add(name, id, storage, sharingKey)
	if err == ErrVolumeIDExist {
		goto random
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// Add adds a volume that was created elsewhere to this node.
//
// If the name exists already, returns ErrVolNameExist.
func (b *Volumes) Add(name string, volID *VolumeID, storage string, sharingKey *SharingKey) (*Volume, error) {
	return b.add(name, volID, storage, sharingKey)
}

func randomVolumeID() (*VolumeID, error) {
	var id VolumeID
	_, err := rand.Read(id[:])
	if err != nil {
		return nil, err
	}
	return &id, nil
}

type Volume struct {
	b  *bolt.Bucket
	id []byte
}

// VolumeID copies the volume ID to out.
//
// out is valid after the transaction.
func (v *Volume) VolumeID(out *VolumeID) {
	copy(out[:], v.id)
}

func (v *Volume) Storage() *VolumeStorage {
	b := v.b.Bucket(volumeStateStorage)
	return &VolumeStorage{b}
}

func (v *Volume) Clock() *VolumeClock {
	b := v.b.Bucket(volumeStateClock)
	return &VolumeClock{b}
}

func (v *Volume) Conflicts() *VolumeConflicts {
	b := v.b.Bucket(volumeStateConflict)
	return &VolumeConflicts{b}
}

// Dirs provides a way of accessing the directory entries stored in
// this volume.
func (v *Volume) Dirs() *Dirs {
	return &Dirs{b: v.b.Bucket(volumeStateDir)}
}

// InodeBucket returns a bolt bucket for storing inodes in.
func (v *Volume) InodeBucket() *bolt.Bucket {
	return v.b.Bucket(volumeStateInode)
}

// SnapBucket returns a bolt bucket for storing snapshots in.
func (v *Volume) SnapBucket() *bolt.Bucket {
	return v.b.Bucket(volumeStateSnap)
}

// Epoch returns the current mutation epoch of the volume.
//
// Returned value is valid after the transaction.
func (v *Volume) Epoch() (clock.Epoch, error) {
	val := v.b.Get(volumeStateEpoch)
	var epoch clock.Epoch
	if err := epoch.UnmarshalBinary(val); err != nil {
		return 0, err
	}
	return epoch, nil
}

func (v *Volume) setEpoch(epoch clock.Epoch) error {
	buf, err := epoch.MarshalBinary()
	if err != nil {
		return err
	}
	if err := v.b.Put(volumeStateEpoch, buf); err != nil {
		return err
	}
	return nil
}

// NextEpoch increments the epoch and returns the new value. The value
// is only safe to use if the transaction commits.
//
// If epoch wraps around, returns ErrVolumeEpochWraparound. This
// should be boil-the-oceans rare.
//
// Returned value is valid after the transaction.
func (v *Volume) NextEpoch() (clock.Epoch, error) {
	epoch, err := v.Epoch()
	if err != nil {
		return epoch, err
	}
	epoch++
	if epoch == 0 {
		return epoch, ErrVolumeEpochWraparound
	}
	if err := v.setEpoch(epoch); err != nil {
		return epoch, err
	}
	return epoch, nil
}
