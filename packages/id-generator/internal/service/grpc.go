// Package service 实现 IDService gRPC 服务。
//
// shadow 路由：服务端 interceptor 把 metadata x-shadow 翻进 ctx；GetID
// 按 ctx 选主 / 影子 buffer，号段空间天然隔离。Snowflake 兜底是无状态的，
// 主 / 影子可共用同一个 Node（worker_id 相同不会冲突，因为 ID 编码本身不
// 区分流量类型；上游消费方按 ctx 落到对应 *_shadow 表，ID 同值不会跨表
// 冲突）。
package service

import (
	"context"

	"github.com/xiongwp/payment-util/shadow"

	"github.com/xiongwp/id-generator/internal/generator"
	pb "github.com/xiongwp/id-generator/internal/proto"
	"github.com/xiongwp/id-generator/internal/segment"
)

type Server struct {
	pb.UnimplementedIDServiceServer
	Sf        *generator.Node
	SegMain   *segment.Buffer // 主流量号段
	SegShadow *segment.Buffer // 影子号段；nil 时 shadow 流量回退到 Sf 兜底
}

func (s *Server) GetID(ctx context.Context, _ *pb.IDRequest) (*pb.IDResponse, error) {
	buf := s.SegMain
	if shadow.IsShadow(ctx) && s.SegShadow != nil {
		buf = s.SegShadow
	}
	id := buf.Next()
	if id == -1 {
		// 号段耗尽（在重新 Load 之前）→ 退到 Snowflake 兜底
		id = s.Sf.NextID()
	}
	return &pb.IDResponse{Id: id}, nil
}
