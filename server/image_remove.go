package server

import (
	"context"
	"fmt"

	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/storage"
	"github.com/cri-o/cri-o/server/cri/types"
)

// RemoveImage removes the image.
func (s *Server) RemoveImage(ctx context.Context, req *types.RemoveImageRequest) error {
	imageRef := ""
	img := req.Image
	if img != nil {
		imageRef = img.Image
	}
	if imageRef == "" {
		return fmt.Errorf("no image specified")
	}
	return s.removeImage(ctx, imageRef)
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

	if err != nil {
		log.Errorf(ctx, "error deleting intermediate image %s: %v", img, err)
	}

	for _, img := range images {
		err = s.StorageImageServer().UntagImage(s.config.SystemContext, img)
		if err != nil {
			log.Debugf(ctx, "Error deleting image %s: %v", img, err)
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
