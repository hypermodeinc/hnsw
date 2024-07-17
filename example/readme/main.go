package main

import (
	"fmt"

	"github.com/hypermodeAI/hnsw"
)

func main() {
	g := hnsw.NewGraph[int]()
	keys := []int{1, 2, 3}
	values := [][]float32{
		{1, 1, 1},
		{1, -1, 0.999},
		{1, 0, -0.5},
	}
	g.Add(
		hnsw.MakeNodes(keys, values)...,
	)

	neighbors := g.Search(
		[]float32{0.5, 0.5, 0.5},
		1,
	)
	fmt.Printf("best friend: %v\n", neighbors[0].Value)
}
