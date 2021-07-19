package server

import (
	"fmt"

	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/storage"
	"golang.org/x/net/context"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// RemoveImage removes the image.
func (s *Server) RemoveImage(ctx context.Context, req *pb.RemoveImageRequest) (*pb.RemoveImageResponse, error) {
	imageRef := ""
	img := req.Image
	if img != nil {
		imageRef = img.Image
	}
	if imageRef == "" {
		return nil, fmt.Errorf("no image specified")
	}
	if err := s.removeImage(ctx, imageRef); err != nil {
		return nil, err
	}
	return &pb.RemoveImageResponse{}, nil
}

func (s *Server) removeImage(ctx context.Context, imageRef string) error {
	var deleted bool
	images, err := s.StorageImageServer().ResolveNames(s.config.SystemContext, imageRef)
	if err != nil {
		if err == storage.ErrCannotParseImageID {
			images = append(images, imageRef)
		} else {
			return err
		}
	}
	for _, img := range images {
		err = s.StorageImageServer().UntagImage(s.config.SystemContext, img)
		if err != nil {
			log.Debugf(ctx, "error deleting image %s: %v", img, err)
			continue
		}
		deleted = true
		break
	}
	if !deleted && err != nil {
		return err
	}
	return nil
}
