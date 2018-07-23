package vcs

import (
	"fmt"
	"strings"

	e "github.com/pkg/errors"
	c "github.com/sahib/brig/catfs/core"
	ie "github.com/sahib/brig/catfs/errors"
	n "github.com/sahib/brig/catfs/nodes"
	capnp_model "github.com/sahib/brig/catfs/nodes/capnp"
	capnp_patch "github.com/sahib/brig/catfs/vcs/capnp"
	capnp "zombiezen.com/go/capnproto2"
)

const (
	// ChangeTypeNone means that a node did not change (compared to HEAD)
	ChangeTypeNone = ChangeType(0)
	// ChangeTypeAdd says that the node was initially added after HEAD.
	ChangeTypeAdd = ChangeType(1 << iota)
	// ChangeTypeModify says that the the node was modified after HEAD
	ChangeTypeModify
	// ChangeTypeMove says that the node was moved after HEAD.
	// Note that Move and Modify may happen at the same time.
	ChangeTypeMove
	// ChangeTypeRemove says that the node was removed after HEAD.
	ChangeTypeRemove
)

// ChangeType is a mask of possible state change events.
type ChangeType uint8

// String will convert a ChangeType to a human readable form
func (ct ChangeType) String() string {
	v := []string{}

	if ct&ChangeTypeAdd != 0 {
		v = append(v, "added")
	}
	if ct&ChangeTypeModify != 0 {
		v = append(v, "modified")
	}
	if ct&ChangeTypeMove != 0 {
		v = append(v, "moved")
	}
	if ct&ChangeTypeRemove != 0 {
		v = append(v, "removed")
	}

	if len(v) == 0 {
		return "none"
	}

	return strings.Join(v, "|")
}

// rule: do not loose content,
//       but we may loose metadata.
//
//   |  a  m  r  mv
// ---------------
// a |  n  n  n  y
// c |  n  n  n  y
// r |  y  y  y  y
// mv|  y  y  y  y
func (ct ChangeType) IsCompatible(ot ChangeType) bool {
	modifyMask := ChangeTypeAdd | ChangeTypeModify
	return ct&modifyMask == 0 || ot&modifyMask == 0
}

///////////////////////////

// Change represents a single change of a node between two commits.
type Change struct {
	// Mask is a bitmask of changes that were made.
	// It describes the change that was made between `Next` to `Head`
	// and which is part of `Head`.
	Mask ChangeType

	// Head is the commit that was the current HEAD when this change happened.
	// Note that this is NOT the commit that contains the change, but the commit before.
	Head *n.Commit

	// Next is the commit that comes before `Head`.
	Next *n.Commit

	// Curr is the node with the attributes at a specific state
	Curr n.ModNode

	// MovedTo is only filled for ghosts that were the source
	// of a move. It's the path of the node it was moved to.
	MovedTo string

	// WasPreviouslyAt points to the place `Curr` was at
	// before a move. On changes without a move this is empty.
	WasPreviouslyAt string
}

func (ch *Change) String() string {
	movedTo := ""
	if len(ch.MovedTo) != 0 {
		movedTo = fmt.Sprintf(" (now %s)", ch.MovedTo)
	}

	prevAt := ""
	if len(ch.WasPreviouslyAt) != 0 {
		prevAt = fmt.Sprintf(" (was %s)", ch.WasPreviouslyAt)
	}

	return fmt.Sprintf("<%s:%s%s%s>", ch.Curr.Path(), ch.Mask, prevAt, movedTo)
}

// Replay applies the change `ch` onto `lkr` by redoing the same operations:
// move, remove, modify, add. Commits are not replayed, everything happens in
// lkr.Status() without creating a new commit.
func (ch *Change) Replay(lkr *c.Linker) error {
	return lkr.Atomic(func() error {
		if ch.Mask&(ChangeTypeModify|ChangeTypeAdd) != 0 {
			currNd := ch.Curr

			// If it's an ghost, unpack it first: It will be added as if it was
			// never a ghost, but since the change mask has the
			// ChangeTypeRemove flag set, it will removed directly after.
			if ch.Curr.Type() == n.NodeTypeGhost {
				currGhost, ok := ch.Curr.(*n.Ghost)
				if !ok {
					return ie.ErrBadNode
				}

				currNd = currGhost.OldNode()
			}

			// Check the type of the old node:
			oldNd, err := lkr.LookupModNode(currNd.Path())
			if err != nil && !ie.IsNoSuchFileError(err) {
				return err
			}

			// If the types are conflicting we have to remove the existing node.
			if oldNd != nil && oldNd.Type() != currNd.Type() {
				_, _, err := c.Remove(lkr, oldNd, false, true)
				if err != nil {
					return e.Wrapf(err, "replay: type-conflict-remove")
				}
			}

			// Something needs to be done based on the type.
			// Either create/update a new file or create a directory.
			switch currNd.(type) {
			case *n.File:
				if _, err := c.StageFromFileNode(lkr, currNd.(*n.File)); err != nil {
					return e.Wrapf(err, "replay: stage")
				}
			case *n.Directory:
				if _, err := c.Mkdir(lkr, currNd.Path(), true); err != nil {
					return e.Wrapf(err, "replay: mkdir")
				}
			default:
				return e.Wrapf(ie.ErrBadNode, "replay: modify")
			}
		}

		if ch.Mask&ChangeTypeMove != 0 {
			if ch.WasPreviouslyAt != "" {
				oldNd, err := lkr.LookupModNode(ch.WasPreviouslyAt)
				if err != nil && !ie.IsNoSuchFileError(err) {
					return err
				}

				if oldNd.Type() != n.NodeTypeGhost {
					if _, _, err := c.Remove(lkr, oldNd, true, true); err != nil {
						return e.Wrap(err, "replay: move: remove old")
					}
				}
			}

			if ch.MovedTo != "" {
				oldNd, err := lkr.LookupModNode(ch.Curr.Path())
				if err != nil && !ie.IsNoSuchFileError(err) {
					return err
				}

				if oldNd != nil {
					if err := c.Move(lkr, oldNd, ch.MovedTo); err != nil {
						return e.Wrapf(err, "replay: move")
					}
				}
			}
		}

		if ch.Mask&ChangeTypeMove != 0 && ch.Curr.Type() != n.NodeTypeGhost {
			oldNd, err := lkr.LookupModNode(ch.WasPreviouslyAt)
			if err != nil && !ie.IsNoSuchFileError(err) {
				return err
			}

			if oldNd.Type() != n.NodeTypeGhost {
				if _, _, err := c.Remove(lkr, oldNd, true, true); err != nil {
					return err
				}
			}
		}

		// We should only remove a node if we're getting a ghost in ch.Curr.
		// Otherwise the node might have been removed and added again.
		if ch.Mask&ChangeTypeRemove != 0 && ch.Curr.Type() == n.NodeTypeGhost {
			currNd, err := lkr.LookupModNode(ch.Curr.Path())
			if err != nil {
				return e.Wrapf(err, "replay: lookup: %v", ch.Curr.Path())
			}

			if currNd.Type() != n.NodeTypeGhost {
				if _, _, err := c.Remove(lkr, currNd, true, true); err != nil {
					return err
				}
			}
		}

		return nil
	})
}

func (ch *Change) toCapnpChange(seg *capnp.Segment, capCh *capnp_patch.Change) error {
	capCurrNd, err := capnp_model.NewNode(seg)
	if err != nil {
		return err
	}

	if err := ch.Curr.ToCapnpNode(seg, capCurrNd); err != nil {
		return err
	}

	capHeadNd, err := capnp_model.NewNode(seg)
	if err != nil {
		return err
	}

	if err := ch.Head.ToCapnpNode(seg, capHeadNd); err != nil {
		return err
	}

	capNextNd, err := capnp_model.NewNode(seg)
	if err != nil {
		return err
	}

	if err := ch.Next.ToCapnpNode(seg, capNextNd); err != nil {
		return err
	}

	if err := capCh.SetCurr(capCurrNd); err != nil {
		return err
	}

	if err := capCh.SetHead(capHeadNd); err != nil {
		return err
	}

	if err := capCh.SetNext(capNextNd); err != nil {
		return err
	}

	if err := capCh.SetMovedTo(ch.MovedTo); err != nil {
		return err
	}

	capCh.SetMask(uint64(ch.Mask))
	return nil

}

func (ch *Change) ToCapnp() (*capnp.Message, error) {
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		return nil, err
	}

	capCh, err := capnp_patch.NewRootChange(seg)
	if err != nil {
		return nil, err
	}

	if err := ch.toCapnpChange(seg, &capCh); err != nil {
		return nil, err
	}

	return msg, nil
}

func (ch *Change) fromCapnpChange(capCh capnp_patch.Change) error {
	capHeadNd, err := capCh.Head()
	if err != nil {
		return err
	}

	ch.Head = &n.Commit{}
	if err := ch.Head.FromCapnpNode(capHeadNd); err != nil {
		return err
	}

	capNextNd, err := capCh.Next()
	if err != nil {
		return err
	}

	ch.Next = &n.Commit{}
	if err := ch.Next.FromCapnpNode(capNextNd); err != nil {
		return err
	}

	capCurrNd, err := capCh.Curr()
	if err != nil {
		return err
	}

	currNd, err := n.CapNodeToNode(capCurrNd)
	if err != nil {
		return err
	}

	currModNd, ok := currNd.(n.ModNode)
	if !ok {
		return e.Wrapf(ie.ErrBadNode, "unmarshalled node is no mod node")
	}

	ch.Curr = currModNd

	referToPath, err := capCh.MovedTo()
	if err != nil {
		return err
	}

	ch.MovedTo = referToPath
	ch.Mask = ChangeType(capCh.Mask())
	return nil
}

func (ch *Change) FromCapnp(msg *capnp.Message) error {
	capCh, err := capnp_patch.ReadRootChange(msg)
	if err != nil {
		return err
	}

	return ch.fromCapnpChange(capCh)
}

// CombineChanges compresses a list of changes (in a lossy way) to one Change.
// The one change should be enough to re-create the changes that were made.
func CombineChanges(changes []*Change) *Change {
	if len(changes) == 0 {
		return nil
	}

	// Only take the latest changes:
	ch := &Change{
		Mask: ChangeType(0),
		Head: changes[0].Head,
		Next: changes[0].Next,
		Curr: changes[0].Curr,
	}

	// If the node moved, save the original path in MovedTo:
	pathChanged := changes[0].Curr.Path() != changes[len(changes)-1].Curr.Path()
	isGhost := changes[0].Curr.Type() == n.NodeTypeGhost

	// Combine the mask:
	for _, change := range changes {
		ch.Mask |= change.Mask
	}

	if ch.Mask&ChangeTypeMove != 0 {
		for idx := len(changes) - 1; idx >= 0; idx-- {
			if refPath := changes[idx].MovedTo; refPath != "" {
				ch.MovedTo = refPath
				break
			}
		}
		for idx := len(changes) - 1; idx >= 0; idx-- {
			if refPath := changes[idx].WasPreviouslyAt; refPath != "" {
				ch.WasPreviouslyAt = refPath
				break
			}
		}
	}

	// If the path did not really change, we do not want
	// to have ChangeTypeMove in the mask. If it's a ghost
	// we should still include it though (for WasPreviouslyAt)
	if !pathChanged && !isGhost {
		ch.Mask &= ^ChangeTypeMove
	}

	// If the last change was not a remove, we do not need to
	if changes[0].Mask&ChangeTypeRemove == 0 && !isGhost {
		ch.Mask &= ^ChangeTypeRemove
	}

	return ch
}
