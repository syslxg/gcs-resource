# GCS Resource forked from frodenas/gcs-resource

add parallel upload support. only works for unversion buckets now.

### `out`: Upload an object to the bucket.

#### Parameters

* `parallel_upload_threshold`: optional. size in MB. defualt to 150. Files bigger than
  this size will be split into multiple (up to 32) parts and upload in parallel.
  - `0`: default value. same as 150
  - negative value: disable parallel mode
  - positive value: size of each trunck, in MB

## Example Configuration

### Resource Type

```yaml
resource_types:
  - name: gcs-resource
    type: docker-image
    source:
      repository: pivotalcfreleng/gcs-resource
```

## DEV

```
  #test
  make
  make integration-tests

  #build
  fly -t releng execute -c ci/tasks/build.yml -i gcs-resource-src=. -o built-resource=.
  docker build -t pivotalcfreleng/gcs-resource .
  docker push pivotalcfreleng/gcs-resource
```
