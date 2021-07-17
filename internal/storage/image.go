package storage

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/pkg/shortnames"
	"github.com/containers/image/v5/signature"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	encconfig "github.com/containers/ocicrypt/config"
	"github.com/containers/podman/v3/pkg/rootless"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/reexec"
	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/dbusmgr"
	"github.com/cri-o/cri-o/utils"
	"github.com/godbus/dbus/v5"
	json "github.com/json-iterator/go"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	minimumTruncatedIDLength = 3
)

var (
	// ErrCannotParseImageID is returned when we try to ResolveNames for an image ID
	ErrCannotParseImageID = errors.New("cannot parse an image ID")
	// ErrImageMultiplyTagged is returned when we try to remove an image that still has multiple names
	ErrImageMultiplyTagged = errors.New("image still has multiple names applied")
)

// ImageResult wraps a subset of information about an image: its ID, its names,
// and the size, if known, or nil if it isn't.
type ImageResult struct {
	ID           string
	Name         string
	RepoTags     []string
	RepoDigests  []string
	Size         *uint64
	Digest       digest.Digest
	ConfigDigest digest.Digest
	User         string
	PreviousName string
	Labels       map[string]string
	OCIConfig    *specs.Image
}

type indexInfo struct {
	name   string
	secure bool
}

// A set of information that we prefer to cache about images, so that we can
// avoid having to reread them every time we need to return information about
// images.
type imageCacheItem struct {
	config       *specs.Image
	size         *uint64
	configDigest digest.Digest
	info         *types.ImageInspectInfo
}

type imageCache map[string]imageCacheItem

type imageLookupService struct {
	DefaultTransport      string
	InsecureRegistryCIDRs []*net.IPNet
	IndexConfigs          map[string]*indexInfo
}

type imageService struct {
	lookup         *imageLookupService
	store          storage.Store
	imageCache     imageCache
	imageCacheLock sync.Mutex
	ctx            context.Context
}

// ImageBeingPulled map[string]bool to keep track of the images haven't done pulling.
var ImageBeingPulled sync.Map

// CgroupPullConfiguration
type CgroupPullConfiguration struct {
	UseNewCgroup bool
	ParentCgroup string
}

// subset of copy.Options that is supported by reexec.
type ImageCopyOptions struct {
	SourceCtx        *types.SystemContext
	DestinationCtx   *types.SystemContext
	OciDecryptConfig *encconfig.DecryptConfig
	ProgressInterval time.Duration
	Progress         chan types.ProgressProperties `json:"-"`
	CgroupPull       CgroupPullConfiguration
}

// ImageServer wraps up various CRI-related activities into a reusable
// implementation.
type ImageServer interface {
	// ListImages returns list of all images which match the filter.
	ListImages(systemContext *types.SystemContext, filter string) ([]ImageResult, error)
	// ImageStatus returns status of an image which matches the filter.
	ImageStatus(systemContext *types.SystemContext, filter string) (*ImageResult, error)
	// PrepareImage returns an Image where the config digest can be grabbed
	// for further analysis. Call Close() on the resulting image.
	PrepareImage(systemContext *types.SystemContext, imageName string) (types.ImageCloser, error)
	// PullImage imports an image from the specified location.
	PullImage(systemContext *types.SystemContext, imageName string, options *ImageCopyOptions) (types.ImageReference, error)
	// UntagImage removes a name from the specified image, and if it was
	// the only name the image had, removes the image.
	UntagImage(systemContext *types.SystemContext, imageName string) error
	// GetStore returns the reference to the storage library Store which
	// the image server uses to hold images, and is the destination used
	// when it's asked to pull an image.
	GetStore() storage.Store
	// ResolveNames takes an image reference and if it's unqualified (w/o hostname),
	// it uses crio's default registries to qualify it.
	ResolveNames(systemContext *types.SystemContext, imageName string) ([]string, error)
}

func (svc *imageService) getRef(name string) (types.ImageReference, error) {
	ref, err := alltransports.ParseImageName(name)
	if err != nil {
		ref2, err2 := istorage.Transport.ParseStoreReference(svc.store, "@"+name)
		if err2 != nil {
			ref3, err3 := istorage.Transport.ParseStoreReference(svc.store, name)
			if err3 != nil {
				return nil, err
			}
			ref2 = ref3
		}
		ref = ref2
	}
	return ref, nil
}

func sortNamesByType(names []string) (bestName string, tags, digests []string) {
	for _, name := range names {
		if len(name) > 72 && name[len(name)-72:len(name)-64] == "@sha256:" {
			digests = append(digests, name)
		} else {
			tags = append(tags, name)
		}
	}
	if len(digests) > 0 {
		bestName = digests[0]
	}
	if len(tags) > 0 {
		bestName = tags[0]
	}
	return bestName, tags, digests
}

func (svc *imageService) makeRepoDigests(knownRepoDigests, tags []string, img *storage.Image) (imageDigest digest.Digest, repoDigests []string) {
	// Look up the image's digests.
	imageDigest = img.Digest
	if imageDigest == "" {
		imgDigest, err := svc.store.ImageBigDataDigest(img.ID, storage.ImageDigestBigDataKey)
		if err != nil || imgDigest == "" {
			return "", knownRepoDigests
		}
		imageDigest = imgDigest
	}
	imageDigests := []digest.Digest{imageDigest}
	for _, anotherImageDigest := range img.Digests {
		if anotherImageDigest != imageDigest {
			imageDigests = append(imageDigests, anotherImageDigest)
		}
	}
	// We only want to supplement what's already explicitly in the list, so keep track of values
	// that we already know.
	digestMap := make(map[string]struct{})
	repoDigests = knownRepoDigests
	for _, repoDigest := range knownRepoDigests {
		digestMap[repoDigest] = struct{}{}
	}
	// For each tagged name, parse the name, and if we can extract a named reference, convert
	// it into a canonical reference using the digest and add it to the list.
	for _, name := range append(tags, knownRepoDigests...) {
		if ref, err2 := reference.ParseNormalizedNamed(name); err2 == nil {
			if name, ok := ref.(reference.Named); ok {
				trimmed := reference.TrimNamed(name)
				for _, imageDigest := range imageDigests {
					if imageRef, err3 := reference.WithDigest(trimmed, imageDigest); err3 == nil {
						if _, ok := digestMap[imageRef.String()]; !ok {
							repoDigests = append(repoDigests, imageRef.String())
							digestMap[imageRef.String()] = struct{}{}
						}
					}
				}
			}
		}
	}
	return imageDigest, repoDigests
}

func (svc *imageService) buildImageCacheItem(systemContext *types.SystemContext, ref types.ImageReference) (imageCacheItem, error) {
	imageFull, err := ref.NewImage(svc.ctx, systemContext)
	if err != nil {
		return imageCacheItem{}, err
	}
	defer imageFull.Close()
	configDigest := imageFull.ConfigInfo().Digest
	imageConfig, err := imageFull.OCIConfig(svc.ctx)
	if err != nil {
		return imageCacheItem{}, err
	}
	size := imageSize(imageFull)

	info, err := imageFull.Inspect(svc.ctx)
	if err != nil {
		return imageCacheItem{}, errors.Wrap(err, "inspecting image")
	}

	return imageCacheItem{
		config:       imageConfig,
		size:         size,
		configDigest: configDigest,
		info:         info,
	}, nil
}

func (svc *imageService) buildImageResult(image *storage.Image, cacheItem imageCacheItem) ImageResult {
	name, tags, digests := sortNamesByType(image.Names)
	imageDigest, repoDigests := svc.makeRepoDigests(digests, tags, image)
	sort.Strings(tags)
	sort.Strings(repoDigests)
	previousName := ""
	if len(image.NamesHistory) > 0 {
		// Remove the tag because we can only keep the name as indicator
		split := strings.SplitN(image.NamesHistory[0], ":", 2)
		if len(split) > 0 {
			previousName = split[0]
		}
	}

	return ImageResult{
		ID:           image.ID,
		Name:         name,
		RepoTags:     tags,
		RepoDigests:  repoDigests,
		Size:         cacheItem.size,
		Digest:       imageDigest,
		ConfigDigest: cacheItem.configDigest,
		User:         cacheItem.config.Config.User,
		PreviousName: previousName,
		Labels:       cacheItem.info.Labels,
		OCIConfig:    cacheItem.config,
	}
}

func (svc *imageService) appendCachedResult(systemContext *types.SystemContext, ref types.ImageReference, image *storage.Image, results []ImageResult, newImageCache imageCache) ([]ImageResult, error) {
	var err error
	svc.imageCacheLock.Lock()
	cacheItem, ok := svc.imageCache[image.ID]
	svc.imageCacheLock.Unlock()
	if !ok {
		cacheItem, err = svc.buildImageCacheItem(systemContext, ref)
		if err != nil {
			return results, err
		}
		if newImageCache == nil {
			svc.imageCacheLock.Lock()
			svc.imageCache[image.ID] = cacheItem
			svc.imageCacheLock.Unlock()
		} else {
			newImageCache[image.ID] = cacheItem
		}
	} else if newImageCache != nil {
		newImageCache[image.ID] = cacheItem
	}

	return append(results, svc.buildImageResult(image, cacheItem)), nil
}

func (svc *imageService) ListImages(systemContext *types.SystemContext, filter string) ([]ImageResult, error) {
	var results []ImageResult
	if filter != "" {
		// we never remove entries from cache unless unfiltered ListImages call is made. Is it safe?
		ref, err := svc.getRef(filter)
		if err != nil {
			return nil, err
		}
		if image, err := istorage.Transport.GetStoreImage(svc.store, ref); err == nil {
			results, err = svc.appendCachedResult(systemContext, ref, image, []ImageResult{}, nil)
			if err != nil {
				return nil, err
			}
		}
	} else {
		images, err := svc.store.Images()
		if err != nil {
			return nil, err
		}
		newImageCache := make(imageCache, len(images))

		storageLayers, err := svc.GetStore().Layers()
		if err != nil {
			return nil, err
		}
		for i := range images {
			image := &images[i]
			// determines if an images is intermediate, and if it is, don't return it
			if isIntermediate(image, storageLayers) {
				continue
			}
			ref, err := istorage.Transport.ParseStoreReference(svc.store, "@"+image.ID)
			if err != nil {
				return nil, err
			}
			results, err = svc.appendCachedResult(systemContext, ref, image, results, newImageCache)
			if err != nil {
				// skip reporting errors if the images haven't finished pulling
				if os.IsNotExist(errors.Cause(err)) {
					donePulling := true
					for _, name := range image.Names {
						if _, ok := ImageBeingPulled.Load(name); ok {
							donePulling = false
							break
						}
					}
					if !donePulling {
						continue
					}
				}
				return nil, err
			}
		}
		// replace image cache with cache we just built
		// this invalidates all stale entries in cache
		svc.imageCacheLock.Lock()
		svc.imageCache = newImageCache
		svc.imageCacheLock.Unlock()
	}
	return results, nil
}

// determines if an images is intermediate
func isIntermediate(image *storage.Image, storageLayers []storage.Layer) bool {
	if len(image.Names) == 0 && !layerIsLeaf(storageLayers, image.TopLayer) {
		return true
	}

	return false
}

// helper function for isIntermediate to determine if a layer is a leaf
func layerIsLeaf(storageLayers []storage.Layer, layer string) bool {
	for i := 0; i < len(storageLayers); i++ {
		if storageLayers[i].Parent == layer {
			return false
		}
	}
	return true
}

func (svc *imageService) ImageStatus(systemContext *types.SystemContext, nameOrID string) (*ImageResult, error) {
	ref, err := svc.getRef(nameOrID)
	if err != nil {
		return nil, err
	}
	image, err := istorage.Transport.GetStoreImage(svc.store, ref)
	if err != nil {
		return nil, err
	}
	cacheItem, err := svc.buildImageCacheItem(systemContext, ref) // Single-use-only, not actually cached
	if err != nil {
		return nil, err
	}

	result := svc.buildImageResult(image, cacheItem)
	return &result, nil
}

func imageSize(img types.Image) *uint64 {
	if sum, err := img.Size(); err == nil {
		usum := uint64(sum)
		return &usum
	}
	return nil
}

// remoteImageReference creates an image reference from an image string
func (svc *imageLookupService) remoteImageReference(imageName string) (types.ImageReference, error) {
	if imageName == "" {
		return nil, storage.ErrNotAnImage
	}

	srcRef, err := alltransports.ParseImageName(imageName)
	if err != nil {
		if svc.DefaultTransport == "" {
			return nil, err
		}
		srcRef2, err2 := alltransports.ParseImageName(svc.DefaultTransport + imageName)
		if err2 != nil {
			return nil, err
		}
		srcRef = srcRef2
	}
	return srcRef, nil
}

// prepareReference creates an image reference from an image string and returns an updated types.SystemContext (never nil) for the image
func (svc *imageLookupService) prepareReference(inputSystemContext *types.SystemContext, imageName string) (*types.SystemContext, types.ImageReference, error) {
	srcRef, err := svc.remoteImageReference(imageName)
	if err != nil {
		return nil, nil, err
	}

	sc := types.SystemContext{}
	if inputSystemContext != nil {
		sc = *inputSystemContext // A shallow copy
	}
	if srcRef.DockerReference() != nil {
		hostname := reference.Domain(srcRef.DockerReference())
		if secure := svc.isSecureIndex(hostname); !secure {
			sc.DockerInsecureSkipTLSVerify = types.OptionalBoolTrue
		}
	}
	return &sc, srcRef, nil
}

func (svc *imageService) PrepareImage(inputSystemContext *types.SystemContext, imageName string) (types.ImageCloser, error) {
	systemContext, srcRef, err := svc.lookup.prepareReference(inputSystemContext, imageName)
	if err != nil {
		return nil, err
	}

	return srcRef.NewImage(svc.ctx, systemContext)
}

// nolint: gochecknoinits
func init() {
	reexec.Register("crio-copy-image", copyImageChild)
}

type copyImageArgs struct {
	Lookup         *imageLookupService
	ImageName      string
	ParentCgroup   string
	SystemContext  *types.SystemContext
	Options        *ImageCopyOptions
	HasCollectMode bool

	StoreOptions storage.StoreOptions
}

// moveSelfToCgroup moves the current process to a new transient cgroup.
func moveSelfToCgroup(cgroup string, hasCollectMode bool) error {
	slice := "system.slice"
	if rootless.IsRootless() {
		slice = "user.slice"
	}

	if cgroup != "" {
		if !strings.Contains(cgroup, ".slice") {
			return fmt.Errorf("invalid systemd cgroup %q", cgroup)
		}
		slice = filepath.Base(cgroup)
	}

	unitName := fmt.Sprintf("crio-pull-image-%d.scope", os.Getpid())

	systemdProperties := []systemdDbus.Property{}
	if hasCollectMode {
		systemdProperties = append(systemdProperties,
			systemdDbus.Property{
				Name:  "CollectMode",
				Value: dbus.MakeVariant("inactive-or-failed"),
			})
	}

	return utils.RunUnderSystemdScope(dbusmgr.NewDbusConnManager(rootless.IsRootless()), os.Getpid(), slice, unitName, systemdProperties...)
}

func copyImageChild() {
	var args copyImageArgs

	if err := json.NewDecoder(os.NewFile(0, "stdin")).Decode(&args); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}

	if err := moveSelfToCgroup(args.ParentCgroup, args.HasCollectMode); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}

	store, err := storage.GetStore(args.StoreOptions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}

	policy, err := signature.DefaultPolicy(args.SystemContext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}

	srcSystemContext, srcRef, destRef, err := args.Lookup.getReferences(args.Options.SourceCtx, store, args.ImageName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}

	progress := make(chan types.ProgressProperties)
	go func() {
		stream := json.NewStream(json.ConfigDefault, os.Stdout, 4096)
		for p := range progress {
			stream.WriteVal(p)
			stream.WriteRaw("\n")
			if err := stream.Flush(); err != nil {
				fmt.Fprintf(os.Stderr, "%v", err)
				os.Exit(1)
			}
		}
	}()

	options := toCopyOptions(args.Options, progress)
	options.SourceCtx = srcSystemContext
	if _, err := copy.Image(context.Background(), policyContext, destRef, srcRef, options); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func toCopyOptions(options *ImageCopyOptions, progress chan types.ProgressProperties) *copy.Options {
	return &copy.Options{
		SourceCtx:        options.SourceCtx,
		DestinationCtx:   options.DestinationCtx,
		OciDecryptConfig: options.OciDecryptConfig,
		ProgressInterval: options.ProgressInterval,
		Progress:         progress,
	}
}

func (svc *imageService) copyImage(systemContext *types.SystemContext, imageName, parentCgroup string, options *ImageCopyOptions) error {
	progress := options.Progress
	dest := imageName
	// the first argument DEST is not used by the re-execed command but it is useful for debugging as it
	// shows in the ps output.
	cmd := reexec.CommandContext(svc.ctx, "crio-copy-image", dest)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "error getting stdout pipe for image copy process")
	}
	defer stdout.Close()

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return errors.Wrap(err, "error getting stderr pipe for image copy process")
	}
	defer stderr.Close()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return errors.Wrap(err, "error getting stdin pipe for image copy process")
	}

	if _, err := alltransports.ParseImageName(imageName); err != nil {
		if svc.lookup.DefaultTransport == "" {
			return err
		}
		imageName = svc.lookup.DefaultTransport + imageName
	}

	stdinArguments := copyImageArgs{
		Lookup:        svc.lookup,
		SystemContext: systemContext,
		Options:       options,
		ImageName:     imageName,
		ParentCgroup:  parentCgroup,
		StoreOptions: storage.StoreOptions{
			RunRoot:            svc.store.RunRoot(),
			GraphRoot:          svc.store.GraphRoot(),
			GraphDriverName:    svc.store.GraphDriverName(),
			GraphDriverOptions: svc.store.GraphOptions(),
			UIDMap:             svc.store.UIDMap(),
			GIDMap:             svc.store.GIDMap(),
		},
		HasCollectMode: node.SystemdHasCollectMode(),
	}

	stdinArguments.Options.Progress = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := json.NewEncoder(stdin).Encode(&stdinArguments); err != nil {
		stdin.Close()
		return errors.Wrap(err, "json encode to pipe failed")
	}
	stdin.Close()

	go func() {
		decoder := json.NewDecoder(bufio.NewReader(stdout))
		if progress != nil {
			defer close(progress)
		}
		for decoder.More() {
			var p types.ProgressProperties
			if err := decoder.Decode(&p); err != nil {
				break
			}

			if progress != nil {
				progress <- p
			}
		}
	}()
	errOutput, errReadAll := ioutil.ReadAll(stderr)
	if err := cmd.Wait(); err != nil {
		if errReadAll == nil && len(errOutput) > 0 {
			return fmt.Errorf("pull image: %s", string(errOutput))
		}
		return err
	}
	return nil
}

func (svc *imageService) PullImage(systemContext *types.SystemContext, imageName string, inputOptions *ImageCopyOptions) (types.ImageReference, error) {
	options := *inputOptions // A shallow copy

	srcSystemContext, srcRef, destRef, err := svc.lookup.getReferences(options.SourceCtx, svc.store, imageName)
	if err != nil {
		return nil, err
	}
	options.SourceCtx = srcSystemContext

	if inputOptions.CgroupPull.UseNewCgroup {
		if err := svc.copyImage(systemContext, imageName, inputOptions.CgroupPull.ParentCgroup, &options); err != nil {
			return nil, err
		}
	} else {
		policy, err := signature.DefaultPolicy(systemContext)
		if err != nil {
			return nil, err
		}
		policyContext, err := signature.NewPolicyContext(policy)
		if err != nil {
			return nil, err
		}

		copyOptions := toCopyOptions(&options, inputOptions.Progress)

		if _, err = copy.Image(svc.ctx, policyContext, destRef, srcRef, copyOptions); err != nil {
			return nil, err
		}
	}
	return destRef, nil
}

// nolint: gocritic
func (svc *imageLookupService) getReferences(inputSystemContext *types.SystemContext, store storage.Store, imageName string) (*types.SystemContext, types.ImageReference, types.ImageReference, error) {
	srcSystemContext, srcRef, err := svc.prepareReference(inputSystemContext, imageName)
	if err != nil {
		return nil, nil, nil, err
	}

	dest := imageName
	if srcRef.DockerReference() != nil {
		dest = srcRef.DockerReference().String()
	}

	destRef, err := istorage.Transport.ParseStoreReference(store, dest)
	if err != nil {
		return nil, nil, nil, err
	}
	return srcSystemContext, srcRef, destRef, nil
}

// recurses through all of the layers that belong to the image and deletes the image
func (svc *imageService) recurseIntermediateLayers(layer *storage.Layer, imgID string) error {
	storageImages, err := svc.store.Images()
	if err != nil {
		return fmt.Errorf("failed to get images when removing intermediate images")
	}

	if layer.Parent == "" {
		return nil
	}

	for i := range storageImages {
		if storageImages[i].TopLayer != layer.ID || storageImages[i].ID == imgID {
			continue
		}

		layer2, err := svc.GetStore().Layer(layer.Parent)
		if err != nil {
			return fmt.Errorf("failed to get layer when removing intermediate images")
		}

		err = svc.recurseIntermediateLayers(layer2, imgID)
		if err != nil {
			return err
		}

		_, err = svc.store.DeleteImage(storageImages[i].ID, true)
		if err != nil {
			return fmt.Errorf("failed to delete image %v", storageImages[i].ID)
		}

		logrus.Infof("removed intermediate image %v", storageImages[i].ID)
	}
	return nil
}

// gets the first layer in the tree to remove, and passes that to helper function recurseIntermediateLayers
func (svc *imageService) removeIntermediate(ref types.ImageReference) error {
	img, err := istorage.Transport.GetStoreImage(svc.store, ref)
	if err != nil {
		return fmt.Errorf("failed to get store image")
	}

	storageLayers, err := svc.GetStore().Layers()
	if err != nil {
		return fmt.Errorf("failed to get storage layers %s", err)
	}

	for i := range storageLayers {
		if storageLayers[i].ID == img.TopLayer {
			err = svc.recurseIntermediateLayers(&storageLayers[i], img.ID)
			if err != nil {
				return fmt.Errorf("failed to delete intermediate images")
			}
		}
	}
	return nil
}

func (svc *imageService) UntagImage(systemContext *types.SystemContext, nameOrID string) error {
	ref, err := svc.getRef(nameOrID)
	if err != nil {
		return err
	}
	img, err := istorage.Transport.GetStoreImage(svc.store, ref)
	if err != nil {
		return err
	}

	if !strings.HasPrefix(img.ID, nameOrID) {
		namedRef, err := svc.lookup.remoteImageReference(nameOrID)
		if err != nil {
			return err
		}

		name := nameOrID
		if namedRef.DockerReference() != nil {
			name = namedRef.DockerReference().String()
		}

		prunedNames := make([]string, 0, len(img.Names))
		for _, imgName := range img.Names {
			if imgName != name && imgName != nameOrID {
				prunedNames = append(prunedNames, imgName)
			}
		}

		if len(prunedNames) > 0 {
			return svc.store.SetNames(img.ID, prunedNames)
		}
	}
	err = svc.removeIntermediate(ref)
	if err != nil {
		return err
	}

	return ref.DeleteImage(svc.ctx, systemContext)
}

func (svc *imageService) GetStore() storage.Store {
	return svc.store
}

func (svc *imageLookupService) isSecureIndex(indexName string) bool {
	if index, ok := svc.IndexConfigs[indexName]; ok {
		return index.secure
	}

	host, _, err := net.SplitHostPort(indexName)
	if err != nil {
		// assume indexName is of the form `host` without the port and go on.
		host = indexName
	}

	addrs, err := net.LookupIP(host)
	if err != nil {
		ip := net.ParseIP(host)
		if ip != nil {
			addrs = []net.IP{ip}
		}

		// if ip == nil, then `host` is neither an IP nor it could be looked up,
		// either because the index is unreachable, or because the index is behind an HTTP proxy.
		// So, len(addrs) == 0 and we're not aborting.
	}

	// Try CIDR notation only if addrs has any elements, i.e. if `host`'s IP could be determined.
	for _, addr := range addrs {
		for _, ipnet := range svc.InsecureRegistryCIDRs {
			// check if the addr falls in the subnet
			if ipnet.Contains(addr) {
				return false
			}
		}
	}

	return true
}

// ResolveNames resolves an image name into a storage image ID or a fully-qualified image name (domain/repo/image:tag).
// Will only return an empty slice if err != nil.
func (svc *imageService) ResolveNames(systemContext *types.SystemContext, imageName string) ([]string, error) {
	// _Maybe_ it's a truncated image ID.  Don't prepend a registry name, then.
	if len(imageName) >= minimumTruncatedIDLength && svc.store != nil {
		if img, err := svc.store.Image(imageName); err == nil && img != nil && strings.HasPrefix(img.ID, imageName) {
			// It's a truncated version of the ID of an image that's present in local storage;
			// we need to expand it.
			return []string{img.ID}, nil
		}
	}
	// This to prevent any image ID to go through this routine
	_, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		if strings.Contains(err.Error(), "cannot specify 64-byte hexadecimal strings") {
			return nil, ErrCannotParseImageID
		}
		return nil, err
	}

	// Disable short name alias mode. Will enable it once we settle on a shortname alias table.
	disabled := types.ShortNameModeDisabled
	systemContext.ShortNameMode = &disabled
	resolved, err := shortnames.Resolve(systemContext, imageName)
	if err != nil {
		return nil, err
	}

	if desc := resolved.Description(); len(desc) > 0 {
		logrus.Info(desc)
	}

	images := make([]string, len(resolved.PullCandidates))
	for i := range resolved.PullCandidates {
		// Strip the tag from ambiguous image references that have a
		// digest as well (e.g.  `image:tag@sha256:123...`).  Such
		// image references are supported by docker but, due to their
		// ambiguity, explicitly not by containers/image.
		ref := resolved.PullCandidates[i].Value
		_, isTagged := ref.(reference.NamedTagged)
		canonical, isDigested := ref.(reference.Canonical)
		if isTagged && isDigested {
			canonical, err = reference.WithDigest(reference.TrimNamed(ref), canonical.Digest())
			if err != nil {
				return nil, err
			}
			ref = canonical
		}
		images[i] = ref.String()
	}

	return images, nil
}

// GetImageService returns an ImageServer that uses the passed-in store, and
// which will prepend the passed-in DefaultTransport value to an image name if
// a name that's passed to its PullImage() method can't be resolved to an image
// in the store and can't be resolved to a source on its own.
func GetImageService(ctx context.Context, sc *types.SystemContext, store storage.Store, defaultTransport string, insecureRegistries []string) (ImageServer, error) {
	if store == nil {
		var err error
		storeOpts, err := storage.DefaultStoreOptions(rootless.IsRootless(), rootless.GetRootlessUID())
		if err != nil {
			return nil, err
		}
		store, err = storage.GetStore(storeOpts)
		if err != nil {
			return nil, err
		}
	}
	ils := &imageLookupService{
		DefaultTransport:      defaultTransport,
		IndexConfigs:          make(map[string]*indexInfo),
		InsecureRegistryCIDRs: make([]*net.IPNet, 0),
	}
	is := &imageService{
		lookup:     ils,
		store:      store,
		imageCache: make(map[string]imageCacheItem),
		ctx:        ctx,
	}

	insecureRegistries = append(insecureRegistries, "127.0.0.0/8")
	// Split --insecure-registry into CIDR and registry-specific settings.
	for _, r := range insecureRegistries {
		// Check if CIDR was passed to --insecure-registry
		_, ipnet, err := net.ParseCIDR(r)
		if err == nil {
			// Valid CIDR.
			is.lookup.InsecureRegistryCIDRs = append(is.lookup.InsecureRegistryCIDRs, ipnet)
		} else {
			// Assume `host:port` if not CIDR.
			is.lookup.IndexConfigs[r] = &indexInfo{
				name:   r,
				secure: false,
			}
		}
	}

	return is, nil
}
