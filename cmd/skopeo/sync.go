package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/directory"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

// syncOptions contains information retrieved from the skopeo sync command line.
type syncOptions struct {
	global            *globalOptions    // Global (not command dependant) skopeo options
	srcImage          *imageOptions     // Source image options
	destImage         *imageDestOptions // Destination image options
	removeSignatures  bool              // Do not copy signatures from the source image
	signByFingerprint string            // Sign the image using a GPG key with the specified fingerprint
	source            string            // Source repository name
	destination       string            // Destination registry name
	scoped            bool              // When true, namespace copied images at destination using the source repository name
}

// repoDescriptor contains information of a single repository used as a sync source.
type repoDescriptor struct {
	DirBasePath  string                 // base path when source is 'dir'
	TaggedImages []types.ImageReference // List of tagged image found for the repository
	Context      *types.SystemContext   // SystemContext for the sync command
}

// tlsVerify is an implementation of the Unmarshaler interface, used to
// customize the unmarshaling behaviour of the tls-verify YAML key.
type tlsVerifyConfig struct {
	skip types.OptionalBool // skip TLS verification check (false by default)
}

// registrySyncConfig contains information about a single registry, read from
// the source YAML file
type registrySyncConfig struct {
	Images      map[string]interface{} // Images map images name to slices or regular expression with the images' tags
	Credentials types.DockerAuthConfig // Username and password used to authenticate with the registry
	TLSVerify   tlsVerifyConfig        `yaml:"tls-verify"` // TLS verification mode (enabled by default)
	CertDir     string                 `yaml:"cert-dir"`   // Path to the TLS certificates of the registry
}

// sourceConfig contains all registries information read from the source YAML file
type sourceConfig map[string]registrySyncConfig

func syncCmd(global *globalOptions) *cobra.Command {
	sharedFlags, sharedOpts := sharedImageFlags()
	srcFlags, srcOpts := dockerImageFlags(global, sharedOpts, "src-", "screds")
	destFlags, destOpts := dockerImageFlags(global, sharedOpts, "dest-", "dcreds")

	opts := syncOptions{
		global:    global,
		srcImage:  srcOpts,
		destImage: &imageDestOptions{imageOptions: destOpts},
	}

	cmd := &cobra.Command{
		Use:   "sync [command options] --src SOURCE-LOCATION --dest DESTINATION-LOCATION SOURCE DESTINATION",
		Short: "Synchronize one or more images from one location to another",
		Long: fmt.Sprint(`Copy all the images from a SOURCE to a DESTINATION.

Allowed SOURCE transports (specified with --src): docker, dir, yaml.
Allowed DESTINATION transports (specified with --dest): docker, dir.

See skopeo-sync(1) for details.
`),
		RunE:    commandAction(opts.run),
		Example: `skopeo sync --src docker --dest dir --scoped registry.example.com/busybox /media/usb`,
	}
	adjustUsage(cmd)
	flags := cmd.Flags()
	flags.BoolVar(&opts.removeSignatures, "remove-signatures", false, "Do not copy signatures from SOURCE images")
	flags.StringVar(&opts.signByFingerprint, "sign-by", "", "Sign the image using a GPG key with the specified `FINGERPRINT`")
	flags.StringVarP(&opts.source, "src", "s", "", "SOURCE transport type")
	flags.StringVarP(&opts.destination, "dest", "d", "", "DESTINATION transport type")
	flags.BoolVar(&opts.scoped, "scoped", false, "Images at DESTINATION are prefix using the full source image path as scope")
	flags.AddFlagSet(&sharedFlags)
	flags.AddFlagSet(&srcFlags)
	flags.AddFlagSet(&destFlags)
	return cmd
}

// unmarshalYAML is the implementation of the Unmarshaler interface method
// method for the tlsVerifyConfig type.
// It unmarshals the 'tls-verify' YAML key so that, when they key is not
// specified, tls verification is enforced.
func (tls *tlsVerifyConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var verify bool
	if err := unmarshal(&verify); err != nil {
		return err
	}

	tls.skip = types.NewOptionalBool(!verify)
	return nil
}

// newSourceConfig unmarshals the provided YAML file path to the sourceConfig type.
// It returns a new unmarshaled sourceConfig object and any error encountered.
func newSourceConfig(yamlFile string) (sourceConfig, error) {
	var cfg sourceConfig
	source, err := ioutil.ReadFile(yamlFile)
	if err != nil {
		return cfg, err
	}
	err = yaml.Unmarshal(source, &cfg)
	if err != nil {
		return cfg, errors.Wrapf(err, "Failed to unmarshal %q", yamlFile)
	}
	return cfg, nil
}

// destinationReference creates an image reference using the provided transport.
// It returns a image reference to be used as destination of an image copy and
// any error encountered.
func destinationReference(destination string, transport string) (types.ImageReference, error) {
	var imageTransport types.ImageTransport

	switch transport {
	case docker.Transport.Name():
		destination = fmt.Sprintf("//%s", destination)
		imageTransport = docker.Transport
	case directory.Transport.Name():
		_, err := os.Stat(destination)
		if err == nil {
			return nil, errors.Errorf(fmt.Sprintf("Refusing to overwrite destination directory %q", destination))
		}
		if !os.IsNotExist(err) {
			return nil, errors.Wrap(err, "Destination directory could not be used")
		}
		// the directory holding the image must be created here
		if err = os.MkdirAll(destination, 0755); err != nil {
			return nil, errors.Wrapf(err, fmt.Sprintf("Error creating directory for image %s",
				destination))
		}
		imageTransport = directory.Transport
	default:
		return nil, errors.Errorf("%q is not a valid destination transport", transport)
	}
	logrus.Debugf("Destination for transport %q: %s", transport, destination)

	destRef, err := imageTransport.ParseReference(destination)
	if err != nil {
		return nil, errors.Wrapf(err, fmt.Sprintf("Cannot obtain a valid image reference for transport %q and reference %q", imageTransport.Name(), destination))
	}

	return destRef, nil
}

// getImageTags retrieves all the tags associated to an image hosted on a
// container registry.
// It returns a string slice of tags and any error encountered.
func getImageTags(ctx context.Context, sysCtx *types.SystemContext, imgRef types.ImageReference) ([]string, error) {
	name := imgRef.DockerReference().Name()
	logrus.WithFields(logrus.Fields{
		"image": name,
	}).Info("Getting tags")
	tags, err := docker.GetRepositoryTags(ctx, sysCtx, imgRef)

	switch err := err.(type) {
	case nil:
		break
	case docker.ErrUnauthorizedForCredentials:
		// Some registries may decide to block the "list all tags" endpoint.
		// Gracefully allow the sync to continue in this case.
		logrus.Warnf("Registry disallows tag list retrieval: %s", err)
		break
	default:
		return tags, errors.Wrapf(err, fmt.Sprintf("Error determining repository tags for image %s", name))
	}

	return tags, nil
}

// isTagSpecified checks if an image name includes a tag and returns any errors
// encountered.
func isTagSpecified(imageName string) (bool, error) {
	normNamed, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		return false, err
	}

	tagged := !reference.IsNameOnly(normNamed)
	logrus.WithFields(logrus.Fields{
		"imagename": imageName,
		"tagged":    tagged,
	}).Info("Tag presence check")
	return tagged, nil
}

// imagesTopCopyFromRepo builds a list of image references from the tags
// found in the source repository.
// It returns an image reference slice with as many elements as the tags found
// and any error encountered.
func imagesToCopyFromRepo(repoReference types.ImageReference, repoName string, sourceCtx *types.SystemContext) ([]types.ImageReference, error) {
	var sourceReferences []types.ImageReference
	tags, err := getImageTags(context.Background(), sourceCtx, repoReference)
	if err != nil {
		return sourceReferences, err
	}

	for _, tag := range tags {
		imageAndTag := fmt.Sprintf("%s:%s", repoName, tag)
		ref, err := docker.ParseReference(imageAndTag)
		if err != nil {
			return nil, errors.Wrapf(err, fmt.Sprintf("Cannot obtain a valid image reference for transport %q and reference %q", docker.Transport.Name(), imageAndTag))
		}
		sourceReferences = append(sourceReferences, ref)
	}
	return sourceReferences, nil
}

// imagesTopCopyFromDir builds a list of image references from the images found
// in the source directory.
// It returns an image reference slice with as many elements as the images found
// and any error encountered.
func imagesToCopyFromDir(dirPath string) ([]types.ImageReference, error) {
	var sourceReferences []types.ImageReference
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "manifest.json" {
			dirname := filepath.Dir(path)
			ref, err := directory.Transport.ParseReference(dirname)
			if err != nil {
				return errors.Wrapf(err, fmt.Sprintf("Cannot obtain a valid image reference for transport %q and reference %q", directory.Transport.Name(), dirname))
			}
			sourceReferences = append(sourceReferences, ref)
			return filepath.SkipDir
		}
		return nil
	})

	if err != nil {
		return sourceReferences,
			errors.Wrapf(err, fmt.Sprintf("Error walking the path %q", dirPath))
	}

	return sourceReferences, nil
}

// imagesTopCopyFromDir builds a list of repository descriptors from the images
// in a registry configuration.
// It returns a repository descriptors slice with as many elements as the images
// found and any error encountered. Each element of the slice is a list of
// tagged image references, to be used as sync source.
func imagesToCopyFromRegistry(registryName string, cfg registrySyncConfig, sourceCtx types.SystemContext) ([]repoDescriptor, error) {
	var repoDescList []repoDescriptor
	for imageName, tags := range cfg.Images {
		repoName := fmt.Sprintf("//%s", path.Join(registryName, imageName))
		logrus.WithFields(logrus.Fields{
			"repo":     imageName,
			"registry": registryName,
		}).Info("Processing repo")

		serverCtx := &sourceCtx
		// override ctx with per-registryName options
		serverCtx.DockerCertPath = cfg.CertDir
		serverCtx.DockerDaemonCertPath = cfg.CertDir
		serverCtx.DockerDaemonInsecureSkipTLSVerify = (cfg.TLSVerify.skip == types.OptionalBoolTrue)
		serverCtx.DockerInsecureSkipTLSVerify = cfg.TLSVerify.skip
		serverCtx.DockerAuthConfig = &cfg.Credentials

		var sourceReferences []types.ImageReference

		switch tags.(type) {
		case []string, []interface{}, nil:
			tagList := make([]string, 0)
			if tagIns, ok := tags.([]interface{}); ok {
				for _, tagValue := range tagIns {
					switch tagValue.(type) {
					case string, int, float64:
						tagList = append(tagList, fmt.Sprintf("%v", tagValue))
					default:
						logrus.WithFields(logrus.Fields{
							"repo":     imageName,
							"registry": registryName,
						}).Error("Error processing repo, skipping")
						logrus.Errorf("Elements can only be strings if they are of type array, wrong value (%v|%T)", tagValue, tagValue)
						continue
					}
				}
			} else {
				// nil is equl full tags
				if tags != nil {
					tagList = tags.([]string)
				}
			}

			for _, tag := range tagList {
				source := fmt.Sprintf("%s:%s", repoName, tag)

				imageRef, err := docker.ParseReference(source)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"tag": source,
					}).Error("Error processing tag, skipping")
					logrus.Errorf("Error getting image reference: %s", err)
					continue
				}
				sourceReferences = append(sourceReferences, imageRef)
			}

			if len(tagList) == 0 {
				logrus.WithFields(logrus.Fields{
					"repo":     imageName,
					"registry": registryName,
				}).Info("Querying registry for image tags")

				imageRef, err := docker.ParseReference(repoName)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"repo":     imageName,
						"registry": registryName,
					}).Error("Error processing repo, skipping")
					logrus.Error(err)
					continue
				}

				sourceReferences, err = imagesToCopyFromRepo(imageRef, repoName, serverCtx)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"repo":     imageName,
						"registry": registryName,
					}).Error("Error processing repo, skipping")
					logrus.Error(err)
					continue
				}
			}

		case string:
			tagReg, err := regexp.Compile(tags.(string))
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"repo":     imageName,
					"registry": registryName,
				}).Error("Error processing repo, skipping")
				logrus.Error(err)
			}

			logrus.WithFields(logrus.Fields{
				"repo":     imageName,
				"registry": registryName,
			}).Info("Querying registry for image tags")

			imageRef, err := docker.ParseReference(repoName)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"repo":     imageName,
					"registry": registryName,
				}).Error("Error processing repo, skipping")
				logrus.Error(err)
				continue
			}

			allSourceReferences, err := imagesToCopyFromRepo(imageRef, repoName, serverCtx)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"repo":     imageName,
					"registry": registryName,
				}).Error("Error processing repo, skipping")
				logrus.Error(err)
				continue
			}

			logrus.WithFields(logrus.Fields{
				"repo":     imageName,
				"registry": registryName,
			}).Infof("Start filtering using the regular expression: %v", tags.(string))
			for _, sReference := range allSourceReferences {
				// get the tag names to match, [1] default is "latest" by .DockerReference().String()
				if tagReg.Match([]byte(strings.Split(sReference.DockerReference().String(), ":")[1])) {
					sourceReferences = append(sourceReferences, sReference)
				}
			}

		default:
			logrus.WithFields(logrus.Fields{
				"repo":     imageName,
				"registry": registryName,
			}).Error("Error processing repo, skipping")
			logrus.Errorf("Tags's type only support []string or regular expression string, wrong type:(%v %T)", tags, tags)
			continue
		}

		if len(sourceReferences) == 0 {
			logrus.WithFields(logrus.Fields{
				"repo":     imageName,
				"registry": registryName,
			}).Warnf("No tags to sync found")
			continue
		}
		repoDescList = append(repoDescList, repoDescriptor{
			TaggedImages: sourceReferences,
			Context:      serverCtx})
	}

	return repoDescList, nil
}

// imagesToCopy retrieves all the images to copy from a specified sync source
// and transport.
// It returns a slice of repository descriptors, where each descriptor is a
// list of tagged image references to be used as sync source, and any error
// encountered.
func imagesToCopy(source string, transport string, sourceCtx *types.SystemContext) ([]repoDescriptor, error) {
	var descriptors []repoDescriptor

	switch transport {
	case docker.Transport.Name():
		desc := repoDescriptor{
			Context: sourceCtx,
		}
		refName := fmt.Sprintf("//%s", source)
		srcRef, err := docker.ParseReference(refName)
		if err != nil {
			return nil, errors.Wrapf(err, fmt.Sprintf("Cannot obtain a valid image reference for transport %q and reference %q", docker.Transport.Name(), refName))
		}
		imageTagged, err := isTagSpecified(source)
		if err != nil {
			return descriptors, err
		}

		if imageTagged {
			desc.TaggedImages = append(desc.TaggedImages, srcRef)
			descriptors = append(descriptors, desc)
			break
		}

		desc.TaggedImages, err = imagesToCopyFromRepo(
			srcRef,
			fmt.Sprintf("//%s", source),
			sourceCtx)

		if err != nil {
			return descriptors, err
		}
		if len(desc.TaggedImages) == 0 {
			return descriptors, errors.Errorf("No images to sync found in %q", source)
		}
		descriptors = append(descriptors, desc)

	case directory.Transport.Name():
		desc := repoDescriptor{
			Context: sourceCtx,
		}

		if _, err := os.Stat(source); err != nil {
			return descriptors, errors.Wrap(err, "Invalid source directory specified")
		}
		desc.DirBasePath = source
		var err error
		desc.TaggedImages, err = imagesToCopyFromDir(source)
		if err != nil {
			return descriptors, err
		}
		if len(desc.TaggedImages) == 0 {
			return descriptors, errors.Errorf("No images to sync found in %q", source)
		}
		descriptors = append(descriptors, desc)

	case "yaml":
		cfg, err := newSourceConfig(source)
		if err != nil {
			return descriptors, err
		}
		for registryName, registryConfig := range cfg {
			if len(registryConfig.Images) == 0 {
				logrus.WithFields(logrus.Fields{
					"registry": registryName,
				}).Warn("No images specified for registry")
				continue
			}

			descs, err := imagesToCopyFromRegistry(registryName, registryConfig, *sourceCtx)
			if err != nil {
				return descriptors, errors.Wrapf(err, "Failed to retrieve list of images from registry %q", registryName)
			}
			descriptors = append(descriptors, descs...)
		}
	}

	return descriptors, nil
}

func (opts *syncOptions) run(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return errorShouldDisplayUsage{errors.New("Exactly two arguments expected")}
	}

	policyContext, err := opts.global.getPolicyContext()
	if err != nil {
		return errors.Wrapf(err, "Error loading trust policy")
	}
	defer policyContext.Destroy()

	// validate source and destination options
	contains := func(val string, list []string) (_ bool) {
		for _, l := range list {
			if l == val {
				return true
			}
		}
		return
	}

	if len(opts.source) == 0 {
		return errors.New("A source transport must be specified")
	}
	if !contains(opts.source, []string{docker.Transport.Name(), directory.Transport.Name(), "yaml"}) {
		return errors.Errorf("%q is not a valid source transport", opts.source)
	}

	if len(opts.destination) == 0 {
		return errors.New("A destination transport must be specified")
	}
	if !contains(opts.destination, []string{docker.Transport.Name(), directory.Transport.Name()}) {
		return errors.Errorf("%q is not a valid destination transport", opts.destination)
	}

	if opts.source == opts.destination && opts.source == directory.Transport.Name() {
		return errors.New("sync from 'dir' to 'dir' not implemented, consider using rsync instead")
	}

	sourceCtx, err := opts.srcImage.newSystemContext()
	if err != nil {
		return err
	}

	sourceArg := args[0]
	srcRepoList, err := imagesToCopy(sourceArg, opts.source, sourceCtx)
	if err != nil {
		return err
	}

	destination := args[1]
	destinationCtx, err := opts.destImage.newSystemContext()
	if err != nil {
		return err
	}

	ctx, cancel := opts.global.commandTimeoutContext()
	defer cancel()

	imagesNumber := 0
	options := copy.Options{
		RemoveSignatures: opts.removeSignatures,
		SignBy:           opts.signByFingerprint,
		ReportWriter:     os.Stdout,
		DestinationCtx:   destinationCtx,
	}

	for _, srcRepo := range srcRepoList {
		options.SourceCtx = srcRepo.Context
		for counter, ref := range srcRepo.TaggedImages {
			var destSuffix string
			switch ref.Transport() {
			case docker.Transport:
				// docker -> dir or docker -> docker
				destSuffix = ref.DockerReference().String()
			case directory.Transport:
				// dir -> docker (we don't allow `dir` -> `dir` sync operations)
				destSuffix = strings.TrimPrefix(ref.StringWithinTransport(), srcRepo.DirBasePath)
				if destSuffix == "" {
					// if source is a full path to an image, have destPath scoped to repo:tag
					destSuffix = path.Base(srcRepo.DirBasePath)
				}
			}

			if !opts.scoped {
				destSuffix = path.Base(destSuffix)
			}

			destRef, err := destinationReference(path.Join(destination, destSuffix), opts.destination)
			if err != nil {
				return err
			}

			logrus.WithFields(logrus.Fields{
				"from": transports.ImageName(ref),
				"to":   transports.ImageName(destRef),
			}).Infof("Copying image tag %d/%d", counter+1, len(srcRepo.TaggedImages))

			_, err = copy.Image(ctx, policyContext, destRef, ref, &options)
			if err != nil {
				return errors.Wrapf(err, fmt.Sprintf("Error copying tag %q", transports.ImageName(ref)))
			}
			imagesNumber++
		}
	}

	logrus.Infof("Synced %d images from %d sources", imagesNumber, len(srcRepoList))
	return nil
}
