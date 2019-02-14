package hare

import "github.com/spacemeshos/go-spacemesh/mesh"

type orphanMock struct {
	f func() []mesh.BlockID
}

func (op *orphanMock) GetOrphanBlocks() []mesh.BlockID {
	if op.f != nil {
		return op.f()
	}
	return []mesh.BlockID{}
}

func (op *orphanMock) GetOrphanBlocksExcept(layer mesh.LayerID) []mesh.BlockID {
	if op.f != nil {
		return op.f()
	}
	return []mesh.BlockID{}
}
