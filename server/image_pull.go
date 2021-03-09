package server

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/types"
	libpodImage "github.com/containers/libpod/libpod/image"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/storage"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/docker/distribution/registry/api/errcode"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

var localRegistryPrefix = libpodImage.DefaultLocalRegistry + "/"

// PullImage pulls a image with authentication config.
func (s *Server) PullImage(ctx context.Context, req *pb.PullImageRequest) (*pb.PullImageResponse, error) {
	// TODO: what else do we need here? (Signatures when the story isn't just pulling from docker://)
	var err error
	image := ""
	img := req.GetImage()
	if img != nil {
		image = img.Image
	}
	log.Infof(ctx, "Pulling image: %s", image)

	pullArgs := pullArguments{image: image}
	if req.GetAuth() != nil {
		username := req.GetAuth().Username
		password := req.GetAuth().Password
		if req.GetAuth().Auth != "" {
			username, password, err = decodeDockerAuth(req.GetAuth().Auth)
			if err != nil {
				log.Debugf(ctx, "error decoding authentication for image %s: %v", image, err)
				return nil, err
			}
		}
		// Specifying a username indicates the user intends to send authentication to the registry.
		if username != "" {
			pullArgs.credentials = types.DockerAuthConfig{
				Username: username,
				Password: password,
			}
		}
	}

	// We use the server's pullOperationsInProgress to record which images are
	// currently being pulled. This allows for avoiding pulling the same image
	// in parallel. Hence, if a given image is currently being pulled, we queue
	// into the pullOperation's waitgroup and wait for the pulling goroutine to
	// unblock us and re-use its results.
	pullOp, pullInProcess := func() (pullOp *pullOperation, inProgress bool) {
		s.pullOperationsLock.Lock()
		defer s.pullOperationsLock.Unlock()
		pullOp, inProgress = s.pullOperationsInProgress[pullArgs]
		if !inProgress {
			pullOp = &pullOperation{}
			s.pullOperationsInProgress[pullArgs] = pullOp
			pullOp.wg.Add(1)
		}
		return pullOp, inProgress
	}()

	if !pullInProcess {
		pullOp.err = errors.New("pullImage was aborted by a Go panic")
		defer func() {
			s.pullOperationsLock.Lock()
			delete(s.pullOperationsInProgress, pullArgs)
			pullOp.wg.Done()
			s.pullOperationsLock.Unlock()
		}()
		pullOp.imageRef, pullOp.err = s.pullImage(ctx, &pullArgs)
	} else {
		// Wait for the pull operation to finish.
		pullOp.wg.Wait()
	}

	if pullOp.err != nil {
		return nil, pullOp.err
	}

	log.Infof(ctx, "Pulled image: %v", pullOp.imageRef)
	return &pb.PullImageResponse{
		ImageRef: pullOp.imageRef,
	}, nil
}

// pullImage performs the actual pull operation of PullImage. Used to separate
// the pull implementation from the pullCache logic in PullImage and improve
// readability and maintainability.
func (s *Server) pullImage(ctx context.Context, pullArgs *pullArguments) (string, error) {
	var err error
	sourceCtx := *s.config.SystemContext // A shallow copy we can modify
	if pullArgs.credentials.Username != "" {
		sourceCtx.DockerAuthConfig = &pullArgs.credentials
	}

	decryptConfig, err := getDecryptionKeys(s.config.DecryptionKeysPath)
	if err != nil {
		return "", err
	}

	var (
		images []string
		pulled string
	)
	images, err = s.StorageImageServer().ResolveNames(s.config.SystemContext, pullArgs.image)
	if err != nil {
		return "", err
	}
	for _, img := range images {
		var tmpImg types.ImageCloser
		tmpImg, err = s.StorageImageServer().PrepareImage(&sourceCtx, img)
		if err != nil {
			// We're not able to find the image remotely, check if it's
			// available locally, but only for localhost/ prefixed ones.
			// This allows pulling localhost/ prefixed images even if the
			// `imagePullPolicy` is set to `Always`.
			if strings.HasPrefix(img, localRegistryPrefix) {
				if _, err := s.StorageImageServer().ImageStatus(
					s.config.SystemContext, img,
				); err == nil {
					pulled = img
					break
				}
			}
			log.Debugf(ctx, "error preparing image %s: %v", img, err)
			tryIncrementImagePullFailureMetric(ctx, img, err)
			continue
		}
		defer tmpImg.Close()

		var storedImage *storage.ImageResult
		storedImage, err = s.StorageImageServer().ImageStatus(s.config.SystemContext, img)
		if err == nil {
			tmpImgConfigDigest := tmpImg.ConfigInfo().Digest
			if tmpImgConfigDigest.String() == "" {
				// this means we are playing with a schema1 image, in which
				// case, we're going to repull the image in any case
				log.Debugf(ctx, "image config digest is empty, re-pulling image")
			} else if tmpImgConfigDigest.String() == storedImage.ConfigDigest.String() {
				log.Debugf(ctx, "image %s already in store, skipping pull", img)
				pulled = img

				// Skipped digests metrics
				tryRecordSkippedMetric(ctx, img, tmpImgConfigDigest.String())

				// Skipped bytes metrics
				if storedImage.Size != nil {
					counter, err := metrics.CRIOImagePullsByNameSkipped.GetMetricWithLabelValues(img)
					if err != nil {
						log.Warnf(ctx, "Unable to write image pull name (skipped) metrics: %v", err)
					} else {
						counter.Add(float64(*storedImage.Size))
					}
				}

				break
			}
			log.Debugf(ctx, "image in store has different ID, re-pulling %s", img)
		}

		// Pull by collecting progress metrics
		progress := make(chan types.ProgressProperties)
		go func() {
			for p := range progress {
				if p.Event == types.ProgressEventSkipped {
					// Skipped digests metrics
					tryRecordSkippedMetric(ctx, img, p.Artifact.Digest.String())
				}
				if p.Artifact.Size > 0 {
					log.Debugf(ctx, "ImagePull (%v): %s (%s): %v bytes (%.2f%%)",
						p.Event, img, p.Artifact.Digest, p.Offset,
						float64(p.Offset)/float64(p.Artifact.Size)*100,
					)
				} else {
					log.Debugf(ctx, "ImagePull (%v): %s (%s): %v bytes",
						p.Event, img, p.Artifact.Digest, p.Offset,
					)
				}

				// Metrics for every digest
				digestCounter, err := metrics.CRIOImagePullsByDigest.GetMetricWithLabelValues(
					img, p.Artifact.Digest.String(), p.Artifact.MediaType,
					fmt.Sprintf("%d", p.Artifact.Size),
				)
				if err != nil {
					log.Warnf(ctx, "Unable to write image pull digest metrics: %v", err)
				} else {
					digestCounter.Add(float64(p.OffsetUpdate))
				}

				// Metrics for the overall image
				nameCounter, err := metrics.CRIOImagePullsByName.GetMetricWithLabelValues(
					img, fmt.Sprintf("%d", imageSize(tmpImg)),
				)
				if err != nil {
					log.Warnf(ctx, "Unable to write image pull name metrics: %v", err)
				} else {
					nameCounter.Add(float64(p.OffsetUpdate))
				}
			}
		}()
		_, err = s.StorageImageServer().PullImage(s.config.SystemContext, img, &copy.Options{
			SourceCtx:        &sourceCtx,
			DestinationCtx:   s.config.SystemContext,
			OciDecryptConfig: decryptConfig,
			ProgressInterval: time.Second,
			Progress:         progress,
		})
		if err != nil {
			log.Debugf(ctx, "error pulling image %s: %v", img, err)
			tryIncrementImagePullFailureMetric(ctx, img, err)
			continue
		}
		pulled = img
		break
	}

	if pulled == "" && err != nil {
		return "", err
	}

	// Update metric for successful image pulls
	nameCounter, err := metrics.CRIOImagePullsSuccesses.GetMetricWithLabelValues(pulled)
	if err != nil {
		log.Warnf(ctx, "Unable to write image pull success metric: %v", err)
	} else {
		nameCounter.Inc()
	}

	status, err := s.StorageImageServer().ImageStatus(s.config.SystemContext, pulled)
	if err != nil {
		return "", err
	}
	imageRef := status.ID
	if len(status.RepoDigests) > 0 {
		imageRef = status.RepoDigests[0]
	}

	return imageRef, nil
}

func tryIncrementImagePullFailureMetric(ctx context.Context, img string, err error) {
	// We try to cover some basic use-cases
	const labelUnknown = "UNKNOWN"
	label := labelUnknown

	// Docker registry errors
	for _, desc := range errcode.GetErrorAllDescriptors() {
		if strings.Contains(err.Error(), desc.Message) {
			label = desc.Value
			break
		}
	}
	if label == labelUnknown {
		if strings.Contains(err.Error(), "connection refused") { // nolint: gocritic
			label = "CONNECTION_REFUSED"
		} else if strings.Contains(err.Error(), "connection timed out") {
			label = "CONNECTION_TIMEOUT"
		} else if strings.Contains(err.Error(), "404 (Not Found)") {
			label = "NOT_FOUND"
		}
	}

	// Update metric for failed image pulls
	nameCounter, err := metrics.CRIOImagePullsFailures.GetMetricWithLabelValues(img, label)
	if err != nil {
		log.Warnf(ctx, "Unable to write image pull failure metric: %v", err)
	} else {
		nameCounter.Inc()
	}
}

func tryRecordSkippedMetric(ctx context.Context, name, digest string) {
	layer := fmt.Sprintf("%s@%s", name, digest)
	log.Debugf(ctx, "Skipped layer %s", layer)

	counter, err := metrics.CRIOImageLayerReuse.GetMetricWithLabelValues(layer)
	if err != nil {
		log.Warnf(ctx, "Unable to write image layer reuse metrics: %v", err)
	} else {
		counter.Inc()
	}
}

func decodeDockerAuth(s string) (user, password string, _ error) {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", err
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		// if it's invalid just skip, as docker does
		return "", "", nil
	}
	user = parts[0]
	password = strings.Trim(parts[1], "\x00")
	return user, password, nil
}

func imageSize(img types.ImageCloser) (size int64) {
	for _, layer := range img.LayerInfos() {
		if layer.Size > 0 {
			size += layer.Size
		} else {
			return -1
		}
	}

	configSize := img.ConfigInfo().Size
	if configSize >= 0 {
		size += configSize
	} else {
		return -1
	}

	return size
}
