package storage_test

import (
	"context"
	"fmt"
	"os"

	"github.com/containers/image/v5/types"
	"github.com/containers/podman/v3/pkg/rootless"
	cs "github.com/containers/storage"
	"github.com/cri-o/cri-o/internal/storage"
	containerstoragemock "github.com/cri-o/cri-o/test/mocks/containerstorage"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	digest "github.com/opencontainers/go-digest"
)

// The actual test suite
var _ = t.Describe("Image", func() {
	// Test constants
	const (
		testDockerRegistry                  = "docker.io"
		testQuayRegistry                    = "quay.io"
		testRedHatRegistry                  = "registry.access.redhat.com"
		testFedoraRegistry                  = "registry.fedoraproject.org"
		testImageName                       = "image"
		testImageAlias                      = "image-for-testing"
		testImageAliasResolved              = "registry.crio.test.com/repo"
		testNormalizedImageName             = "docker.io/library/image:latest" // Keep in sync with testImageName!
		testSHA256                          = "2a03a6059f21e150ae84b0973863609494aad70f0a80eaeb64bddd8d92465812"
		testImageWithTagAndDigest           = "image:latest@sha256:" + testSHA256
		testNormalizedImageWithTagAndDigest = "docker.io/library/image:latest@sha256:" + testSHA256
	)

	var (
		mockCtrl  *gomock.Controller
		storeMock *containerstoragemock.MockStore

		// The system under test
		sut storage.ImageServer

		// The empty system context
		ctx *types.SystemContext
	)

	// Prepare the system under test
	BeforeEach(func() {
		// Setup the mocks
		mockCtrl = gomock.NewController(GinkgoT())
		storeMock = containerstoragemock.NewMockStore(mockCtrl)

		// Setup the SUT
		var err error
		ctx = &types.SystemContext{
			SystemRegistriesConfPath: t.MustTempFile("registries"),
		}

		sut, err = storage.GetImageService(
			context.Background(), ctx, storeMock, "docker://", []string{},
		)
		Expect(err).To(BeNil())
		Expect(sut).NotTo(BeNil())
	})
	AfterEach(func() {
		mockCtrl.Finish()
		Expect(os.Remove(ctx.SystemRegistriesConfPath)).To(BeNil())
	})

	mockGetRef := func() mockSequence {
		return inOrder(
			// parseStoreReference ("@"+testImageName) will fail, recognizing it as an invalid image ID
			storeMock.EXPECT().Image(testImageName).
				Return(&cs.Image{ID: testSHA256}, nil),

			mockParseStoreReference(storeMock, testImageName),
		)
	}

	t.Describe("GetImageService", func() {
		It("should succeed to retrieve an image service", func() {
			// Given
			// When
			imageService, err := storage.GetImageService(
				context.Background(), nil, storeMock, "", []string{},
			)

			// Then
			Expect(err).To(BeNil())
			Expect(imageService).NotTo(BeNil())
		})

		It("should succeed with custom registries.conf", func() {
			// Given
			// When
			imageService, err := storage.GetImageService(
				context.Background(),
				&types.SystemContext{
					SystemRegistriesConfPath: "../../test/registries.conf",
				},
				storeMock, "", []string{},
			)

			// Then
			Expect(err).To(BeNil())
			Expect(imageService).NotTo(BeNil())
		})

		It("should fail to retrieve an image service without storage", func() {
			// Given
			storeOptions, err := cs.DefaultStoreOptions(rootless.IsRootless(), rootless.GetRootlessUID())
			Expect(err).To(BeNil())
			storeOptions.GraphRoot = ""

			// When
			_, err = cs.GetStore(storeOptions)

			// Then
			Expect(err).NotTo(BeNil())
		})
	})

	t.Describe("GetStore", func() {
		It("should succeed to retrieve the store", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Delete(gomock.Any()).Return(nil),
			)

			// When
			store := sut.GetStore()

			// Then
			Expect(store).NotTo(BeNil())
			Expect(store.Delete("")).To(BeNil())
		})
	})

	t.Describe("ResolveNames", func() {
		It("should succeed to resolve", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: "id"}, nil),
			)

			// When
			names, err := sut.ResolveNames(
				&types.SystemContext{
					SystemRegistriesConfPath: "../../test/registries.conf",
				},
				testImageName,
			)

			// Then
			Expect(err).To(BeNil())
			Expect(names).To(Equal([]string{
				testQuayRegistry + "/" + testImageName + ":latest",
				testRedHatRegistry + "/" + testImageName + ":latest",
				testFedoraRegistry + "/" + testImageName + ":latest",
				testDockerRegistry + "/library/" + testImageName + ":latest",
			}))
		})

		It("should succeed to resolve to a short-name alias", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: "id"}, nil),
			)

			// When
			names, err := sut.ResolveNames(
				&types.SystemContext{
					SystemRegistriesConfPath: "../../test/registries.conf",
				},
				testImageAlias,
			)

			// Then
			Expect(err).To(BeNil())
			Expect(names).To(Equal([]string{
				testImageAliasResolved + ":latest",
			}))
		})
		It("should succeed to resolve with full qualified image name", func() {
			// Given
			const imageName = "docker.io/library/busybox:latest"
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: "id"}, nil),
			)

			// When
			names, err := sut.ResolveNames(ctx, imageName)

			// Then
			Expect(err).To(BeNil())
			Expect(len(names)).To(Equal(1))
			Expect(names[0]).To(Equal(imageName))
		})

		It("should succeed to resolve image name with tag and digest", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: "id"}, nil),
			)

			// When
			names, err := sut.ResolveNames(
				&types.SystemContext{
					SystemRegistriesConfPath: "../../test/registries.conf",
				},
				testImageWithTagAndDigest,
			)
			// Then
			Expect(err).To(BeNil())
			Expect(names).To(Equal([]string{
				testQuayRegistry + "/" + testImageName + "@sha256:" + testSHA256,
				testRedHatRegistry + "/" + testImageName + "@sha256:" + testSHA256,
				testFedoraRegistry + "/" + testImageName + "@sha256:" + testSHA256,
				testDockerRegistry + "/library/" + testImageName + "@sha256:" + testSHA256,
			}))
		})

		It("should succeed to resolve fully qualified image name with tag and digest", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: "id"}, nil),
			)

			// When
			names, err := sut.ResolveNames(ctx, testNormalizedImageWithTagAndDigest)

			// Then
			Expect(err).To(BeNil())
			Expect(names).To(Equal([]string{
				testDockerRegistry + "/library/" + testImageName + "@sha256:" + testSHA256,
			}))
		})

		It("should succeed to resolve with a local copy", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: testImageName}, nil),
			)

			// When
			names, err := sut.ResolveNames(nil, testImageName)

			// Then
			Expect(err).To(BeNil())
			Expect(len(names)).To(Equal(1))
			Expect(names[0]).To(Equal(testImageName))
		})

		It("should fail to resolve with invalid image id", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: testImageName}, nil),
			)

			// When
			names, err := sut.ResolveNames(ctx, testSHA256)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(err).To(Equal(storage.ErrCannotParseImageID))
			Expect(names).To(BeNil())
		})

		It("should fail to resolve with invalid registry name", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: testImageName}, nil),
			)

			// When
			names, err := sut.ResolveNames(ctx, "camelCaseName")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(names).To(BeNil())
		})

		It("should fail to resolve without configured registries", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Image(gomock.Any()).
					Return(&cs.Image{ID: "id"}, nil),
			)

			// Create an empty file for the registries config path
			sut, err := storage.GetImageService(context.Background(),
				ctx, storeMock, "", []string{},
			)
			Expect(err).To(BeNil())
			Expect(sut).NotTo(BeNil())

			// When
			names, err := sut.ResolveNames(
				&types.SystemContext{
					SystemRegistriesConfPath: "/dev/null",
				},
				testImageName,
			)

			// Then
			Expect(err).NotTo(BeNil())
			errString := fmt.Sprintf("short-name %q did not resolve to an alias and no unqualified-search registries are defined in %q", testImageName, "/dev/null")
			Expect(err.Error()).To(Equal(errString))
			Expect(names).To(BeNil())
		})
	})

	t.Describe("UntagImage", func() {
		It("should succeed to untag an image", func() {
			// Given
			inOrder(
				mockGetRef(),
				mockGetStoreImage(storeMock, testNormalizedImageName, testSHA256),
				mockGetStoreImage(storeMock, testNormalizedImageName, testSHA256),
				storeMock.EXPECT().Layers().Return([]cs.Layer{}, nil),
				mockResolveImage(storeMock, testNormalizedImageName, testSHA256),
				storeMock.EXPECT().DeleteImage(testSHA256, true).
					Return(nil, nil),
			)

			// When
			err := sut.UntagImage(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).To(BeNil())
		})

		It("should fail to untag an image with invalid name", func() {
			// Given
			// When
			err := sut.UntagImage(&types.SystemContext{}, "")

			// Then
			Expect(err).NotTo(BeNil())
		})

		It("should fail to untag an image that can't be found", func() {
			// Given
			inOrder(
				mockGetRef(),
				mockGetStoreImage(storeMock, testNormalizedImageName, ""),
			)

			// When
			err := sut.UntagImage(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).NotTo(BeNil())
		})

		It("should fail to untag an image with a docker:// reference", func() {
			// Given
			const imageName = "docker://localhost/busybox:latest"
			inOrder(
				mockGetStoreImage(storeMock, "localhost/busybox:latest", testSHA256),
				mockGetStoreImage(storeMock, "localhost/busybox:latest", testSHA256),
				storeMock.EXPECT().Layers().Return([]cs.Layer{}, nil),
			)

			// When
			err := sut.UntagImage(&types.SystemContext{}, imageName)

			// Then
			Expect(err).NotTo(BeNil()) // FIXME: this actually fails because it tries to untag the image at the docker://localhost registry!
		})

		It("should fail to untag an image with a docker:// digest reference", func() {
			// Given
			const imageName = "docker://localhost/busybox@sha256:" + testSHA256
			inOrder(
				mockGetStoreImage(storeMock, "localhost/busybox@sha256:"+testSHA256, testSHA256),

				mockGetStoreImage(storeMock, "localhost/busybox@sha256:"+testSHA256, testSHA256),
				storeMock.EXPECT().Layers().Return([]cs.Layer{}, nil),
			)

			// When
			err := sut.UntagImage(&types.SystemContext{}, imageName)

			// Then
			Expect(err).NotTo(BeNil()) // FIXME: this actually fails because it tries to untag the image at the docker://localhost registry!
		})

		It("should fail to untag an image with multiple names", func() {
			// Given
			inOrder(
				mockGetRef(),
				storeMock.EXPECT().Image(testNormalizedImageName).
					Return(&cs.Image{
						ID:    testSHA256,
						Names: []string{testNormalizedImageName, "localhost/b:latest", "localhost/c:latest"},
					}, nil),

				storeMock.EXPECT().SetNames(testSHA256, []string{"localhost/b:latest", "localhost/c:latest"}).Return(t.TestError),
			)

			// When
			err := sut.UntagImage(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).NotTo(BeNil())
		})

		It("should remove intermediate images", func() {
			// Given
			storeMock.EXPECT().Layers().Return([]cs.Layer{
				{ID: "topLayer1", Parent: "topLayer2"},
				{ID: "topLayer2", Parent: "topLayer3"},
				{ID: "topLayer3", Parent: ""},
			}, nil).AnyTimes()

			storeMock.EXPECT().GraphOptions().Return([]string{}).AnyTimes()
			storeMock.EXPECT().GraphDriverName().Return("").AnyTimes()
			storeMock.EXPECT().GraphRoot().Return("").AnyTimes()
			storeMock.EXPECT().RunRoot().Return("").AnyTimes()

			storeMock.EXPECT().Image(testSHA256[0:62]+"01").
				Return(&cs.Image{
					ID:       testSHA256[0:62] + "01",
					TopLayer: "topLayer1",
				}, nil).AnyTimes()
			storeMock.EXPECT().Images().Return(
				[]cs.Image{
					{ID: testSHA256[0:62] + "01", TopLayer: "topLayer1"},
					{ID: testSHA256[0:62] + "02", TopLayer: "topLayer2"},
					{ID: testSHA256[0:62] + "03", TopLayer: "topLayer3"},
				},
				nil).AnyTimes()
			storeMock.EXPECT().Layer("topLayer2").Return(&cs.Layer{
				ID: "topLayer2", Parent: "",
			}, nil).AnyTimes()

			storeMock.EXPECT().DeleteImage(testSHA256[0:62]+"01", true).Return(nil, nil).AnyTimes()
			storeMock.EXPECT().DeleteImage(testSHA256[0:62]+"02", true).Return(nil, nil).AnyTimes()

			err := sut.UntagImage(&types.SystemContext{}, testSHA256[0:62]+"01")
			Expect(err).To(BeNil())
		})
	})

	t.Describe("ImageStatus", func() {
		It("should succeed to get the image status with digest", func() {
			// Given
			inOrder(
				mockGetRef(),
				// storage.Transport.GetStoreImage:
				storeMock.EXPECT().Image(testNormalizedImageName).
					Return(&cs.Image{
						ID: testSHA256,
						Names: []string{
							testNormalizedImageName,
							"localhost/a@sha256:" + testSHA256,
							"localhost/b@sha256:" + testSHA256,
							"localhost/c:latest",
						},
					}, nil),
				// buildImageCacheItem
				mockNewImage(storeMock, testNormalizedImageName, testSHA256),
				// makeRepoDigests
				storeMock.EXPECT().ImageBigDataDigest(testSHA256, gomock.Any()).
					Return(digest.Digest("a:"+testSHA256), nil),
			)

			// When
			res, err := sut.ImageStatus(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).To(BeNil())
			Expect(res).NotTo(BeNil())
		})

		It("should fail to get on wrong reference", func() {
			// Given
			// When
			res, err := sut.ImageStatus(&types.SystemContext{}, "")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail to get on missing store image", func() {
			// Given
			inOrder(
				mockGetRef(),
				mockGetStoreImage(storeMock, testNormalizedImageName, ""),
			)

			// When
			res, err := sut.ImageStatus(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail to get on corrupt image", func() {
			// Given
			inOrder(
				mockGetRef(),
				mockGetStoreImage(storeMock, testNormalizedImageName, testSHA256),
				// In buildImageCacheItem, storageReference.NewImage fails reading the manifest:
				mockResolveImage(storeMock, testNormalizedImageName, testSHA256),
				storeMock.EXPECT().ImageBigData(testSHA256, gomock.Any()).
					Return(nil, t.TestError),
			)

			// When
			res, err := sut.ImageStatus(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})
	})

	t.Describe("ListImages", func() {
		It("should succeed to list images without filter", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Images().Return([]cs.Image{}, nil),
				storeMock.EXPECT().Layers().Return([]cs.Layer{}, nil),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, "")

			// Then
			Expect(err).To(BeNil())
			Expect(len(res)).To(Equal(0))
		})

		It("should succeed to list multiple images without filter", func() {
			// Given
			mockLoop := func() mockSequence {
				return inOrder(
					mockParseStoreReference(storeMock, "@"+testSHA256),
					// buildImageCacheItem:
					mockNewImage(storeMock, testSHA256, testSHA256),
					// makeRepoDigests:
					storeMock.EXPECT().ImageBigDataDigest(testSHA256, gomock.Any()).
						Return(digest.Digest(""), nil),
				)
			}
			inOrder(
				storeMock.EXPECT().Images().Return(
					[]cs.Image{
						{ID: testSHA256, Names: []string{"a", "b", "c@sha256:" + testSHA256}},
						{ID: testSHA256},
					},
					nil),
				storeMock.EXPECT().Layers().Return([]cs.Layer{}, nil),
				mockLoop(),
				mockLoop(),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, "")

			// Then
			Expect(err).To(BeNil())
			Expect(len(res)).To(Equal(2))
		})

		It("should succeed to list images with filter", func() {
			// Given
			inOrder(
				mockGetRef(),
				mockGetStoreImage(storeMock, testNormalizedImageName, testSHA256),
				// buildImageCacheItem:
				mockNewImage(storeMock, testNormalizedImageName, testSHA256),
				// makeRepoDigests:
				storeMock.EXPECT().ImageBigDataDigest(testSHA256, gomock.Any()).
					Return(digest.Digest(""), nil),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).To(BeNil())
			Expect(len(res)).To(Equal(1))
			Expect(res[0].ID).To(Equal(testSHA256))
		})

		It("should succeed to list images on failure to access an image", func() {
			// Given
			inOrder(
				mockGetRef(),
				mockGetStoreImage(storeMock, testNormalizedImageName, ""),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).To(BeNil())
			Expect(len(res)).To(Equal(0))
		})

		It("should fail to list images with filter an invalid reference", func() {
			// Given
			gomock.InOrder(
				// parseStoreReference("@wrong://image") tries this before failing in parseNormalizedNamed:
				storeMock.EXPECT().Image("wrong://image").Return(nil, cs.ErrImageUnknown),
			)
			// When
			res, err := sut.ListImages(&types.SystemContext{}, "wrong://image")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail to list images with filter on failing appendCachedResult", func() {
			// Given
			inOrder(
				mockGetRef(),
				mockGetStoreImage(storeMock, testNormalizedImageName, testSHA256),
				// in buildImageCacheItem, NewImage is failing:
				mockResolveImage(storeMock, testNormalizedImageName, testSHA256),
				storeMock.EXPECT().ImageBigData(testSHA256, gomock.Any()).
					Return(nil, t.TestError),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, testImageName)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail to list images without a filter on failing store", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Images().Return(nil, t.TestError),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, "")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail to list multiple images without filter on invalid image ID in results", func() {
			// Given
			gomock.InOrder(
				storeMock.EXPECT().Images().Return(
					[]cs.Image{{ID: ""}}, nil),
				storeMock.EXPECT().Layers().Return([]cs.Layer{}, nil),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, "")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail to list multiple images without filter on append", func() {
			// Given
			inOrder(
				storeMock.EXPECT().Images().Return(
					[]cs.Image{{ID: testSHA256}}, nil),
				storeMock.EXPECT().Layers().Return([]cs.Layer{}, nil),
				mockParseStoreReference(storeMock, "@"+testSHA256),
				storeMock.EXPECT().Image(gomock.Any()).
					Return(nil, t.TestError),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, "")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should succeed to not list intermediate images", func() {
			// Given
			mockLoop := func() mockSequence {
				return inOrder(
					mockParseStoreReference(storeMock, "@"+testSHA256),
					// buildImageCacheItem:
					mockNewImage(storeMock, testSHA256, testSHA256),
					// makeRepoDigests:
					storeMock.EXPECT().ImageBigDataDigest(testSHA256, gomock.Any()).
						Return(digest.Digest(""), nil),
				)
			}
			inOrder(
				storeMock.EXPECT().Images().Return(
					[]cs.Image{
						{ID: testSHA256, Names: []string{"a", "b", "c@sha256:" + testSHA256}},
						{ID: testSHA256},
					},
					nil),
				// creates a layer with the parent of one of the images just returned
				storeMock.EXPECT().Layers().Return([]cs.Layer{{Parent: testSHA256}}, nil),
				mockLoop(),
				mockLoop(),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, "")

			// Then
			Expect(err).To(BeNil())
			Expect(len(res)).To(Equal(2))
		})

		It("should succeed to omit an intermediate image", func() {
			// Given
			mockLoop := func() mockSequence {
				return inOrder(
					mockParseStoreReference(storeMock, "@"+testSHA256),
					// buildImageCacheItem:
					mockNewImage(storeMock, testSHA256, testSHA256),
					// makeRepoDigests:
					storeMock.EXPECT().ImageBigDataDigest(testSHA256, gomock.Any()).
						Return(digest.Digest(""), nil),
				)
			}
			inOrder(
				storeMock.EXPECT().Images().Return(
					[]cs.Image{
						{ID: testSHA256, Names: []string{"a", "b", "c@sha256:" + testSHA256}},
						{ID: "ID2", TopLayer: "testTopLayer1"},
						{ID: "ID3", TopLayer: "testTopLayer2"},
					},
					nil),
				// creates a layer with the parent of one of the images just returned
				storeMock.EXPECT().Layers().Return([]cs.Layer{
					{ID: "ID2", Parent: "testTopLayer1"},
					{ID: "ID3", Parent: "testTopLayer2"},
				}, nil),
				mockLoop(),
			)

			// When
			res, err := sut.ListImages(&types.SystemContext{}, "")

			// Then
			Expect(err).To(BeNil())
			Expect(len(res)).To(Equal(1))
		})
	})

	t.Describe("PrepareImage", func() {
		It("should succeed with testimage", func() {
			// Given
			const imageName = "tarball:../../test/testdata/image.tar"

			// When
			res, err := sut.PrepareImage(&types.SystemContext{}, imageName)

			// Then
			Expect(err).To(BeNil())
			Expect(res).NotTo(BeNil())
		})

		It("should fail on invalid image name", func() {
			// Given
			// When
			res, err := sut.PrepareImage(&types.SystemContext{}, "")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})
	})

	t.Describe("PullImage", func() {
		It("should fail on invalid image name", func() {
			// Given
			// When
			res, err := sut.PullImage(&types.SystemContext{}, "",
				&storage.ImageCopyOptions{})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail on invalid policy path", func() {
			// Given
			// When
			res, err := sut.PullImage(&types.SystemContext{
				SignaturePolicyPath: "/not-existing",
			}, "", &storage.ImageCopyOptions{})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail on copy image", func() {
			// Given
			const imageName = "docker://localhost/busybox:latest"
			mockParseStoreReference(storeMock, "localhost/busybox:latest")

			// When
			res, err := sut.PullImage(&types.SystemContext{
				SignaturePolicyPath: "../../test/policy.json",
			}, imageName, &storage.ImageCopyOptions{})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})

		It("should fail on canonical copy image", func() {
			// Given
			const imageName = "docker://localhost/busybox@sha256:" + testSHA256
			mockParseStoreReference(storeMock, "localhost/busybox@sha256:"+testSHA256)

			// When
			res, err := sut.PullImage(&types.SystemContext{
				SignaturePolicyPath: "../../test/policy.json",
			}, imageName, &storage.ImageCopyOptions{})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(BeNil())
		})
	})
})
