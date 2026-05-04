package service

import (
	"context"

	"github.com/xiongwp/id-generator/internal/generator"
	pb "github.com/xiongwp/id-generator/internal/proto"
	"github.com/xiongwp/id-generator/internal/segment"
)

type Server struct {
	pb.UnimplementedIDServiceServer
	Sf  *generator.Node
	Seg *segment.Buffer
}

func (s *Server) GetID(ctx context.Context, _ *pb.IDRequest) (*pb.IDResponse, error) {
	id := s.Seg.Next()
	if id == -1 {
		id = s.Sf.NextID()
	}
	return &pb.IDResponse{Id: id}, nil
}
