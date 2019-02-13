package consensus

import (
	"github.com/spacemeshos/go-spacemesh/log"
	"github.com/spacemeshos/go-spacemesh/mesh"
)

type Algorithm struct {
	Tortoise
	callback func(mesh.LayerID)
}

type Tortoise interface {
	handleIncomingLayer(ll *mesh.Layer)
}

func NewAlgorithm(trtl Tortoise) *Algorithm {
	return &Algorithm{Tortoise: trtl}
}

func (alg *Algorithm) RegisterLayerCallback(callback func(mesh.LayerID)) {
	alg.callback = callback
}

func (alg *Algorithm) HandleLateBlock(b *mesh.Block) {
	log.Info("received block with layer Id %v block id: %v ", b.Layer(), b.ID())
}

func (alg *Algorithm) HandleIncomingLayer(ll *mesh.Layer) {
	alg.Tortoise.handleIncomingLayer(ll)
	alg.callback(ll.Index())
}

func CreateGenesisLayer() *mesh.Layer {
	log.Info("Creating genesis")
	bl := &mesh.Block{
		Id:         mesh.BlockID(0),
		LayerIndex: 0,
		Data:       []byte("genesis"),
	}
	l := mesh.NewLayer(Genesis)
	l.AddBlock(bl)
	return l
}
