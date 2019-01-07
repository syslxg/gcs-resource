package gcsresource

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"golang.org/x/oauth2"
	oauthgoogle "golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/storage/v1"
	"gopkg.in/cheggaaa/pb.v1"
)

//go:generate counterfeiter -o fakes/fake_gcsclient.go . GCSClient
type GCSClient interface {
	BucketObjects(bucketName string, prefix string) ([]string, error)
	ObjectGenerations(bucketName string, objectPath string) ([]int64, error)
	DownloadFile(bucketName string, objectPath string, generation int64, localPath string) error
	UploadFile(bucketName string, objectPath string, objectContentType string, localPath string, predefinedACL string, parallelUploadThreshold int) (int64, error)
	URL(bucketName string, objectPath string, generation int64) (string, error)
	DeleteObject(bucketName string, objectPath string, generation int64) error
	GetBucketObjectInfo(bucketName, objectPath string) (*storage.Object, error)
}

type gcsclient struct {
	storageService *storage.Service
	progressOutput io.Writer
}

func NewGCSClient(
	progressOutput io.Writer,
	jsonKey string,
) (GCSClient, error) {
	var err error
	var storageClient *http.Client
	var userAgent = "gcs-resource/0.0.1"

	if jsonKey != "" {
		storageJwtConf, err := oauthgoogle.JWTConfigFromJSON([]byte(jsonKey), storage.DevstorageFullControlScope)
		if err != nil {
			return &gcsclient{}, err
		}
		storageClient = storageJwtConf.Client(oauth2.NoContext)
	} else {
		storageClient, err = oauthgoogle.DefaultClient(oauth2.NoContext, storage.DevstorageFullControlScope)
		if err != nil {
			return &gcsclient{}, err
		}
	}

	storageService, err := storage.New(storageClient)
	if err != nil {
		return &gcsclient{}, err
	}
	storageService.UserAgent = userAgent

	return &gcsclient{
		storageService: storageService,
		progressOutput: progressOutput,
	}, nil
}

func (gcsclient *gcsclient) BucketObjects(bucketName string, prefix string) ([]string, error) {
	bucketObjects, err := gcsclient.getBucketObjects(bucketName, prefix)
	if err != nil {
		return []string{}, err
	}

	return bucketObjects, nil
}

func (gcsclient *gcsclient) ObjectGenerations(bucketName string, objectPath string) ([]int64, error) {
	isBucketVersioned, err := gcsclient.getBucketVersioning(bucketName)
	if err != nil {
		return []int64{}, err
	}

	if !isBucketVersioned {
		return []int64{}, errors.New("bucket is not versioned")
	}

	objectGenerations, err := gcsclient.getObjectGenerations(bucketName, objectPath)
	if err != nil {
		return []int64{}, err
	}

	return objectGenerations, nil
}

func (gcsclient *gcsclient) DownloadFile(bucketName string, objectPath string, generation int64, localPath string) error {
	isBucketVersioned, err := gcsclient.getBucketVersioning(bucketName)
	if err != nil {
		return err
	}

	if !isBucketVersioned && generation != 0 {
		return errors.New("bucket is not versioned")
	}

	getCall := gcsclient.storageService.Objects.Get(bucketName, objectPath)
	if generation != 0 {
		getCall = getCall.Generation(generation)
	}

	object, err := getCall.Do()
	if err != nil {
		return err
	}

	localFile, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer localFile.Close()

	progress := gcsclient.newProgressBar(int64(object.Size))
	progress.Start()
	defer progress.Finish()

	response, err := getCall.Download()
	if err != nil {
		return err
	}
	defer response.Body.Close()

	reader := progress.NewProxyReader(response.Body)
	_, err = io.Copy(localFile, reader)
	if err != nil {
		return err
	}

	return nil
}

func (gcsclient *gcsclient) planParallelUpload(fileSize int64, trunkSize int64) (int64, int64) {
	threads := fileSize / trunkSize
	if fileSize%trunkSize != 0 {
		threads++
	}

	if threads > 32 {
		threads = 32
		trunkSize = fileSize / 32
		fmt.Fprintf(os.Stderr, "Warning: Only up to 32 threads are supported. Parameter parallel_upload_threshold is ignored. Using %d MB for each thread.\n", trunkSize>>20)
	}

	return threads, trunkSize
}

func (gcsclient *gcsclient) UploadFile(bucketName string, objectPath string, objectContentType string, localPath string, predefinedACL string, parallelUploadThreshold int) (int64, error) {
	isBucketVersioned, err := gcsclient.getBucketVersioning(bucketName)
	if err != nil {
		return 0, err
	}

	stat, err := os.Stat(localPath)
	if err != nil {
		return 0, err
	}
	fileSize := stat.Size()
	parallelMode := false
	threads := int64(1)
	trunkSize := int64(parallelUploadThreshold) << 20
	if parallelUploadThreshold > 0 {
		threads, trunkSize = gcsclient.planParallelUpload(fileSize, trunkSize)
		fmt.Fprintf(os.Stderr, "parallel upload %v. using %d threads. \n", parallelMode, threads)
	}
	if threads > 1 {
		parallelMode = true
	}
	progress := gcsclient.newProgressBar(fileSize)
	progress.Start()
	defer progress.Finish()
	var mediaOptions []googleapi.MediaOption
	if objectContentType != "" {
		mediaOptions = append(mediaOptions, googleapi.ContentType(objectContentType))
	}

	if parallelMode {
		readers := make([]io.Reader, threads)
		sourceObjects := make([]*storage.ComposeRequestSourceObjects, threads)
		errChannel := make(chan error)
		for i := int64(0); i < threads; i++ {
			localFile, err := os.Open(localPath)
			//TODO: close the files
			if err != nil {
				return 0, err
			}
			localFile.Seek(trunkSize*i, 0)
			if i == threads-1 {
				readers[i] = localFile
			} else {
				readers[i] = io.LimitReader(localFile, trunkSize)
			}
			partName := objectPath + ".part" + strconv.Itoa(int(i))
			object := &storage.Object{
				Name: partName,

				ContentType: objectContentType,
			}
			sourceObjects[i] = &storage.ComposeRequestSourceObjects{Name: partName}
			insertCall := gcsclient.storageService.Objects.Insert(bucketName, object).Media(progress.NewProxyReader(readers[i]), mediaOptions...)
			if predefinedACL != "" {
				insertCall = insertCall.PredefinedAcl(predefinedACL)
			}

			go func() {
				_, err = insertCall.Do()
				errChannel <- err
			}()

		}

		for i := int64(0); i < threads; i++ {
			err = <-errChannel
			if err != nil {
				return 0, err
			}
		}

		progress.Finish()
		fmt.Fprintf(os.Stderr, "\n\nSending compose request to merge the files...\n")
		composeReqest := &storage.ComposeRequest{
			SourceObjects: sourceObjects,
		}
		composeCall := gcsclient.storageService.Objects.Compose(bucketName, objectPath, composeReqest)
		_, err = composeCall.Do()
		if err != nil {
			return 0, err
		}

		fmt.Fprintf(os.Stderr, "Cleanup...\n")
		for i := int64(0); i < threads; i++ {
			partName := objectPath + ".part" + strconv.Itoa(int(i))
			err = gcsclient.storageService.Objects.Delete(bucketName, partName).Do()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Failed to delete file %s: %v\n", partName, err)
			}
		}
		return 0, nil
	} else {
		localFile, err := os.Open(localPath)
		if err != nil {
			return 0, err
		}
		defer localFile.Close()

		object := &storage.Object{
			Name:        objectPath,
			ContentType: objectContentType,
		}

		insertCall := gcsclient.storageService.Objects.Insert(bucketName, object).Media(progress.NewProxyReader(localFile), mediaOptions...)
		if predefinedACL != "" {
			insertCall = insertCall.PredefinedAcl(predefinedACL)
		}

		uploadedObject, err := insertCall.Do()
		if err != nil {
			return 0, err
		}

		if isBucketVersioned {
			return uploadedObject.Generation, nil
		}

		return 0, nil
	}
}

func (gcsclient *gcsclient) URL(bucketName string, objectPath string, generation int64) (string, error) {
	getCall := gcsclient.storageService.Objects.Get(bucketName, objectPath)
	if generation != 0 {
		getCall = getCall.Generation(generation)
	}

	_, err := getCall.Do()
	if err != nil {
		return "", err
	}

	var url string
	if generation != 0 {
		url = fmt.Sprintf("gs://%s/%s#%d", bucketName, objectPath, generation)
	} else {
		url = fmt.Sprintf("gs://%s/%s", bucketName, objectPath)
	}

	return url, nil
}

func (gcsclient *gcsclient) DeleteObject(bucketName string, objectPath string, generation int64) error {
	deleteCall := gcsclient.storageService.Objects.Delete(bucketName, objectPath)
	if generation != 0 {
		deleteCall = deleteCall.Generation(generation)
	}

	err := deleteCall.Do()
	if err != nil {
		return err
	}

	return nil
}

func (gcsclient *gcsclient) GetBucketObjectInfo(bucketName, objectPath string) (*storage.Object, error) {
	getCall := gcsclient.storageService.Objects.Get(bucketName, objectPath)
	object, err := getCall.Do()
	if err != nil {
		return nil, err
	}

	return object, nil
}

func (gcsclient *gcsclient) getBucketObjects(bucketName string, prefix string) ([]string, error) {
	var bucketObjects []string

	pageToken := ""
	for {
		listCall := gcsclient.storageService.Objects.List(bucketName)
		listCall = listCall.PageToken(pageToken)
		listCall = listCall.Prefix(prefix)
		listCall = listCall.Versions(false)

		objects, err := listCall.Do()
		if err != nil {
			return bucketObjects, err
		}

		for _, object := range objects.Items {
			bucketObjects = append(bucketObjects, object.Name)
		}

		if objects.NextPageToken != "" {
			pageToken = objects.NextPageToken
		} else {
			break
		}
	}

	return bucketObjects, nil
}

func (gcsclient *gcsclient) getBucketVersioning(bucketName string) (bool, error) {
	bucket, err := gcsclient.storageService.Buckets.Get(bucketName).Do()
	if err != nil {
		return false, err
	}

	if bucket.Versioning != nil {
		return bucket.Versioning.Enabled, nil
	}

	return false, nil
}

func (gcsclient *gcsclient) getObjectGenerations(bucketName string, objectPath string) ([]int64, error) {
	var objectGenerations []int64

	pageToken := ""
	for {
		listCall := gcsclient.storageService.Objects.List(bucketName)
		listCall = listCall.PageToken(pageToken)
		listCall = listCall.Prefix(objectPath)
		listCall = listCall.Versions(true)

		objects, err := listCall.Do()
		if err != nil {
			return objectGenerations, err
		}

		for _, object := range objects.Items {
			if object.Name == objectPath {
				objectGenerations = append(objectGenerations, object.Generation)
			}
		}

		if objects.NextPageToken != "" {
			pageToken = objects.NextPageToken
		} else {
			break
		}
	}

	return objectGenerations, nil
}

func (gcsclient *gcsclient) newProgressBar(total int64) *pb.ProgressBar {
	progress := pb.New64(total)

	progress.Output = gcsclient.progressOutput
	progress.ShowSpeed = true
	progress.Units = pb.U_BYTES
	progress.NotPrint = true

	return progress.SetWidth(80)
}
