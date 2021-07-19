package storage

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/pkg/sysregistriesv2"
	"github.com/containers/image/v5/signature"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/containers/libpod/pkg/rootless"
	"github.com/containers/storage"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	minimumTruncatedIDLength = 3
)

var (
	// ErrCannotParseImageID is returned when we try to ResolveNames for an image ID
	ErrCannotParseImageID = errors.New("cannot parse an image ID")
	// ErrImageMultiplyTagged is returned when we try to remove an image that still has multiple names
	ErrImageMultiplyTagged = errors.New("image still has multiple names applied")
	// ErrNoRegistriesConfigured is returned when there are no registries configured in /etc/crio.conf#additional_registries
	ErrNoRegistriesConfigured = errors.New(`no registries configured while trying to pull an unqualified image, add at least one in either /etc/crio/crio.conf or /etc/containers/registries.conf`)
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

type imageService struct {
	store                       storage.Store
	defaultTransport            string
	insecureRegistryCIDRs       []*net.IPNet
	indexConfigs                map[string]*indexInfo
	unqualifiedSearchRegistries []string
	imageCache                  imageCache
	imageCacheLock              sync.Mutex
	ctx                         context.Context
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
	PullImage(systemContext *types.SystemContext, imageName string, options *copy.Options) (types.ImageReference, error)
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
		for i := range images {
			image := &images[i]
			ref, err := istorage.Transport.ParseStoreReference(svc.store, "@"+image.ID)
			if err != nil {
				return nil, err
			}
			results, err = svc.appendCachedResult(systemContext, ref, image, results, newImageCache)
			if err != nil {
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
func (svc *imageService) remoteImageReference(imageName string) (types.ImageReference, error) {
	if imageName == "" {
		return nil, storage.ErrNotAnImage
	}

	srcRef, err := alltransports.ParseImageName(imageName)
	if err != nil {
		if svc.defaultTransport == "" {
			return nil, err
		}
		srcRef2, err2 := alltransports.ParseImageName(svc.defaultTransport + imageName)
		if err2 != nil {
			return nil, err
		}
		srcRef = srcRef2
	}
	return srcRef, nil
}

// prepareReference creates an image reference from an image string and returns an updated types.SystemContext (never nil) for the image
func (svc *imageService) prepareReference(inputSystemContext *types.SystemContext, imageName string) (*types.SystemContext, types.ImageReference, error) {
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
	systemContext, srcRef, err := svc.prepareReference(inputSystemContext, imageName)
	if err != nil {
		return nil, err
	}

	return srcRef.NewImage(svc.ctx, systemContext)
}

func (svc *imageService) PullImage(systemContext *types.SystemContext, imageName string, inputOptions *copy.Options) (types.ImageReference, error) {
	policy, err := signature.DefaultPolicy(systemContext)
	if err != nil {
		return nil, err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return nil, err
	}

	options := *inputOptions // A shallow copy
	srcSystemContext, srcRef, err := svc.prepareReference(options.SourceCtx, imageName)
	if err != nil {
		return nil, err
	}
	options.SourceCtx = srcSystemContext

	dest := imageName
	if srcRef.DockerReference() != nil {
		dest = srcRef.DockerReference().String()
	}
	destRef, err := istorage.Transport.ParseStoreReference(svc.store, dest)
	if err != nil {
		return nil, err
	}
	_, err = copy.Image(svc.ctx, policyContext, destRef, srcRef, &options)
	if err != nil {
		return nil, err
	}
	return destRef, nil
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
		namedRef, err := svc.remoteImageReference(nameOrID)
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

	return ref.DeleteImage(svc.ctx, systemContext)
}

func (svc *imageService) GetStore() storage.Store {
	return svc.store
}

func (svc *imageService) isSecureIndex(indexName string) bool {
	if index, ok := svc.indexConfigs[indexName]; ok {
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
		for _, ipnet := range svc.insecureRegistryCIDRs {
			// check if the addr falls in the subnet
			if ipnet.Contains(addr) {
				return false
			}
		}
	}

	return true
}

func splitDockerDomain(name string) (domain, remainder string) {
	i := strings.IndexRune(name, '/')
	if i == -1 || (!strings.ContainsAny(name[:i], ".:") && name[:i] != "localhost") {
		domain, remainder = "", name
	} else {
		domain, remainder = name[:i], name[i+1:]
	}
	return
}

// imageNamesWithDigestOrTag strips the tag from ambiguous image references that have a digest as well (e.g. `image:tag@sha256:123...`).
// Such image references are supported by docker but, due to their ambiguity,
// explicitly not by containers/image.
func imageNamesWithDigestOrTag(images []string) ([]string, error) {
	normalized := make([]string, len(images))
	for i, imageName := range images {
		ref, err := reference.ParseNormalizedNamed(imageName)
		if err != nil {
			return nil, err
		}
		_, isTagged := ref.(reference.NamedTagged)
		canonical, isDigested := ref.(reference.Canonical)
		if isTagged && isDigested {
			canonical, err = reference.WithDigest(reference.TrimNamed(ref), canonical.Digest())
			if err != nil {
				return nil, err
			}
			imageName = canonical.String()
		}
		normalized[i] = imageName
	}
	return normalized, nil
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

	domain, remainder := splitDockerDomain(imageName)
	if domain != "" {
		return imageNamesWithDigestOrTag([]string{imageName})
	}
	unqualifiedSearchRegistries, err := sysregistriesv2.UnqualifiedSearchRegistries(systemContext)
	if err != nil {
		return nil, err
	}
	// we got an unqualified image here, we can't go ahead w/o registries configured
	// properly.
	if len(svc.unqualifiedSearchRegistries) == 0 && len(unqualifiedSearchRegistries) == 0 {
		return nil, ErrNoRegistriesConfigured
	}
	// this means we got an image in the form of "busybox"
	// we need to use additional registries...
	// normalize the unqualified image to be domain/repo/image...
	images := []string{}
	for _, r := range append(svc.unqualifiedSearchRegistries, unqualifiedSearchRegistries...) {
		rem := remainder
		if r == "docker.io" && !strings.ContainsRune(remainder, '/') {
			rem = "library/" + rem
		}
		image := r + "/" + rem
		images = append(images, image)
	}
	return imageNamesWithDigestOrTag(images)
}

// GetImageService returns an ImageServer that uses the passed-in store, and
// which will prepend the passed-in defaultTransport value to an image name if
// a name that's passed to its PullImage() method can't be resolved to an image
// in the store and can't be resolved to a source on its own.
func GetImageService(ctx context.Context, sc *types.SystemContext, store storage.Store, defaultTransport string, insecureRegistries, registries []string) (ImageServer, error) {
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

	is := &imageService{
		store:                 store,
		defaultTransport:      defaultTransport,
		indexConfigs:          make(map[string]*indexInfo),
		insecureRegistryCIDRs: make([]*net.IPNet, 0),
		imageCache:            make(map[string]imageCacheItem),
		ctx:                   ctx,
	}

	if len(registries) != 0 {
		seenRegistries := make(map[string]bool, len(registries))
		cleanRegistries := []string{}
		for _, r := range registries {
			if seenRegistries[r] {
				continue
			}
			cleanRegistries = append(cleanRegistries, r)
			seenRegistries[r] = true
		}

		is.unqualifiedSearchRegistries = cleanRegistries
	} else {
		systemRegistries, err := sysregistriesv2.UnqualifiedSearchRegistries(sc)
		if err != nil {
			return nil, err
		}
		is.unqualifiedSearchRegistries = systemRegistries
	}

	insecureRegistries = append(insecureRegistries, "127.0.0.0/8")
	// Split --insecure-registry into CIDR and registry-specific settings.
	for _, r := range insecureRegistries {
		// Check if CIDR was passed to --insecure-registry
		_, ipnet, err := net.ParseCIDR(r)
		if err == nil {
			// Valid CIDR.
			is.insecureRegistryCIDRs = append(is.insecureRegistryCIDRs, ipnet)
		} else {
			// Assume `host:port` if not CIDR.
			is.indexConfigs[r] = &indexInfo{
				name:   r,
				secure: false,
			}
		}
	}

	return is, nil
}
