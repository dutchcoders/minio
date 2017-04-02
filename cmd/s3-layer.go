/*
 * Minio Cloud Storage, (C) 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"crypto/sha256"
	"hash"
	"io"
	"net/http"
	"path"

	"encoding/hex"

	minio "github.com/minio/minio-go"
)

// Convert Minio errors to minio object layer errors.
func s3ToObjectError(err error, params ...string) error {
	if err == nil {
		return nil
	}

	e, ok := err.(*Error)
	if !ok {
		// Code should be fixed if this function is called without doing traceError()
		// Else handling different situations in this function makes this function complicated.
		errorIf(err, "Expected type *Error")
		return err
	}

	err = e.e

	bucket := ""
	object := ""
	if len(params) >= 1 {
		bucket = params[0]
	}
	if len(params) == 2 {
		object = params[1]
	}

	minioErr, ok := err.(minio.ErrorResponse)
	if !ok {
		// We don't interpret non Minio errors. As minio errors will
		// have StatusCode to help to convert to object errors.
		return e
	}

	switch minioErr.Code {
	case "BucketAlreadyOwnedByYou":
		err = BucketAlreadyOwnedByYou{}
	case "BucketNotEmpty":
		err = BucketNotEmpty{}
	case "InvalidBucketName":
		err = BucketNameInvalid{Bucket: bucket}
	case "NoSuchBucket":
		err = BucketNotFound{Bucket: bucket}
	case "NoSuchKey":
		if object != "" {
			err = ObjectNotFound{Bucket: bucket, Object: object}
		} else {
			err = BucketNotFound{Bucket: bucket}
		}
	case "XMinioInvalidObjectName":
		err = ObjectNameInvalid{}
	case "AccessDenied":
		err = PrefixAccessDenied{
			Bucket: bucket,
			Object: object,
		}
	}

	e.e = err
	return e
}

// s3Gateway - Implements gateway for S3 and Minio blob storage.
type s3Gateway struct {
	Client     *minio.Core
	anonClient *minio.Core
}

// newS3Gateway returns s3 gatewaylayer
func newS3Gateway(endpoint string, https bool, accessKey, secretKey string) (GatewayLayer, error) {
	// Initialize minio client object.
	client, err := minio.NewCore(endpoint, accessKey, secretKey, https)
	if err != nil {
		return nil, err
	}

	anonClient, err := minio.NewCore(endpoint, "", "", https)
	if err != nil {
		return nil, err
	}

	return &s3Gateway{
		Client:     client,
		anonClient: anonClient,
	}, nil
}

// Shutdown - save any gateway metadata to disk
// if necessary and reload upon next restart.
func (l *s3Gateway) Shutdown() error {
	// TODO
	return nil
}

// StorageInfo - Not relevant to S3 backend.
func (l *s3Gateway) StorageInfo() StorageInfo {
	return StorageInfo{}
}

// MakeBucket - Create a new container on S3 backend.
func (l *s3Gateway) MakeBucket(location, bucket string) error {
	err := l.Client.MakeBucket(bucket, location)
	if err != nil {
		return s3ToObjectError(traceError(err), bucket)
	}
	return err
}

// GetBucketInfo - Get bucket metadata..
func (l *s3Gateway) GetBucketInfo(bucket string) (BucketInfo, error) {
	buckets, err := l.Client.ListBuckets()
	if err != nil {
		return BucketInfo{}, s3ToObjectError(traceError(err), bucket)
	}

	for _, bi := range buckets {
		if bi.Name != bucket {
			continue
		}

		return BucketInfo{
			Name:    bi.Name,
			Created: bi.CreationDate,
		}, nil
	}

	return BucketInfo{}, traceError(BucketNotFound{Bucket: bucket})
}

// ListBuckets - Lists all S3 buckets
func (l *s3Gateway) ListBuckets() ([]BucketInfo, error) {
	buckets, err := l.Client.ListBuckets()
	if err != nil {
		return nil, err
	}

	b := make([]BucketInfo, len(buckets))
	for i, bi := range buckets {
		b[i] = BucketInfo{
			Name:    bi.Name,
			Created: bi.CreationDate,
		}
	}

	return b, err
}

// DeleteBucket - delete a bucket on S3
func (l *s3Gateway) DeleteBucket(bucket string) error {
	err := l.Client.RemoveBucket(bucket)
	if err != nil {
		return s3ToObjectError(traceError(err), bucket)
	}
	return nil
}

// ListObjects - lists all blobs in S3 bucket filtered by prefix
func (l *s3Gateway) ListObjects(bucket string, prefix string, marker string, delimiter string, maxKeys int) (ListObjectsInfo, error) {
	result, err := l.Client.ListObjects(bucket, prefix, marker, delimiter, maxKeys)
	if err != nil {
		return ListObjectsInfo{}, s3ToObjectError(traceError(err), bucket)
	}

	return fromMinioClientListBucketResult(bucket, result), nil
}

// fromMinioClientListBucketResult - convert minio ListBucketResult to ListObjectsInfo
func fromMinioClientListBucketResult(bucket string, result minio.ListBucketResult) ListObjectsInfo {
	objects := make([]ObjectInfo, len(result.Contents))

	for i, oi := range result.Contents {
		objects[i] = fromMinioClientObjectInfo(bucket, oi)
	}

	return ListObjectsInfo{
		IsTruncated: result.IsTruncated,
		NextMarker:  result.NextMarker,
		Prefixes: []string{
			result.Prefix,
		},
		Objects: objects,
	}
}

// GetObject - reads an object from S3. Supports additional
// parameters like offset and length which are synonymous with
// HTTP Range requests.
//
// startOffset indicates the starting read location of the object.
// length indicates the total length of the object.
func (l *s3Gateway) GetObject(bucket string, key string, startOffset int64, length int64, writer io.Writer) error {
	object, err := l.Client.GetObject(bucket, key)
	if err != nil {
		return s3ToObjectError(traceError(err), bucket, key)
	}

	defer object.Close()

	object.Seek(startOffset, io.SeekStart)
	if _, err := io.CopyN(writer, object, length); err != nil {
		return s3ToObjectError(traceError(err), bucket, key)
	}

	return nil
}

// fromMinioClientObjectInfo -- converts minio ObjectInfo to gateway ObjectInfo
func fromMinioClientObjectInfo(bucket string, oi minio.ObjectInfo) ObjectInfo {
	userDefined := fromMinioClientMetadata(oi.Metadata)
	userDefined["Content-Type"] = oi.ContentType

	return ObjectInfo{
		Bucket:          bucket,
		Name:            oi.Key,
		ModTime:         oi.LastModified,
		Size:            oi.Size,
		MD5Sum:          oi.ETag,
		UserDefined:     userDefined,
		ContentType:     oi.ContentType,
		ContentEncoding: oi.Metadata.Get("Content-Encoding"),
	}
}

// GetObjectInfo - reads object info and replies back ObjectInfo
func (l *s3Gateway) GetObjectInfo(bucket string, object string) (objInfo ObjectInfo, err error) {
	oi, err := l.Client.StatObject(bucket, object)
	if err != nil {
		return ObjectInfo{}, s3ToObjectError(traceError(err), bucket, object)
	}

	return fromMinioClientObjectInfo(bucket, oi), nil
}

// PutObject - Create a new blob with the incoming data,
func (l *s3Gateway) PutObject(bucket string, object string, size int64, data io.Reader, metadata map[string]string, sha256sum string) (ObjectInfo, error) {
	var sha256Writer hash.Hash

	teeReader := data
	if sha256sum != "" {
		sha256Writer = sha256.New()
		teeReader = io.TeeReader(data, sha256Writer)
	}

	delete(metadata, "md5Sum")

	err := l.Client.PutObject(bucket, object, size, teeReader, toMinioClientMetadata(metadata))
	if err != nil {
		return ObjectInfo{}, s3ToObjectError(traceError(err), bucket, object)
	}

	if sha256sum != "" {
		newSHA256sum := hex.EncodeToString(sha256Writer.Sum(nil))
		if newSHA256sum != sha256sum {
			l.Client.RemoveObject(bucket, object)
			return ObjectInfo{}, traceError(SHA256Mismatch{})
		}
	}

	oi, err := l.GetObjectInfo(bucket, object)
	if err != nil {
		return ObjectInfo{}, s3ToObjectError(traceError(err), bucket, object)
	}

	return oi, nil
}

// CopyObject - Copies a blob from source container to destination container.
func (l *s3Gateway) CopyObject(srcBucket string, srcObject string, destBucket string, destObject string, metadata map[string]string) (ObjectInfo, error) {
	err := l.Client.CopyObject(destBucket, destObject, path.Join(srcBucket, srcObject), minio.CopyConditions{})
	if err != nil {
		return ObjectInfo{}, s3ToObjectError(traceError(err), srcBucket, srcObject)
	}

	oi, err := l.GetObjectInfo(destBucket, destObject)
	if err != nil {
		return ObjectInfo{}, s3ToObjectError(traceError(err), destBucket, destObject)
	}

	return oi, nil
}

// DeleteObject - Deletes a blob in bucket
func (l *s3Gateway) DeleteObject(bucket string, object string) error {
	err := l.Client.RemoveObject(bucket, object)
	if err != nil {
		return s3ToObjectError(traceError(err), bucket, object)
	}

	return nil
}

// fromMinioClientUploadMetadata converts ObjectMultipartInfo to uploadMetadata
func fromMinioClientUploadMetadata(omi minio.ObjectMultipartInfo) uploadMetadata {
	return uploadMetadata{
		Object:    omi.Key,
		UploadID:  omi.UploadID,
		Initiated: omi.Initiated,
	}
}

// fromMinioClientListMultipartsInfo converts minio ListMultipartUploadsResult to ListMultipartsInfo
func fromMinioClientListMultipartsInfo(lmur minio.ListMultipartUploadsResult) ListMultipartsInfo {
	uploads := make([]uploadMetadata, len(lmur.Uploads))

	for i, um := range lmur.Uploads {
		uploads[i] = fromMinioClientUploadMetadata(um)
	}

	commonPrefixes := make([]string, len(lmur.CommonPrefixes))
	for i, cp := range lmur.CommonPrefixes {
		commonPrefixes[i] = cp.Prefix
	}

	return ListMultipartsInfo{
		KeyMarker:          lmur.KeyMarker,
		UploadIDMarker:     lmur.UploadIDMarker,
		NextKeyMarker:      lmur.NextKeyMarker,
		NextUploadIDMarker: lmur.NextUploadIDMarker,
		MaxUploads:         int(lmur.MaxUploads),
		IsTruncated:        lmur.IsTruncated,
		Uploads:            uploads,
		Prefix:             lmur.Prefix,
		Delimiter:          lmur.Delimiter,
		CommonPrefixes:     commonPrefixes,
		EncodingType:       lmur.EncodingType,
	}

}

// ListMultipartUploads - lists all multipart uploads.
func (l *s3Gateway) ListMultipartUploads(bucket string, prefix string, keyMarker string, uploadIDMarker string, delimiter string, maxUploads int) (ListMultipartsInfo, error) {
	result, err := l.Client.ListMultipartUploads(bucket, prefix, keyMarker, uploadIDMarker, delimiter, maxUploads)
	if err != nil {
		return ListMultipartsInfo{}, err
	}

	return fromMinioClientListMultipartsInfo(result), nil
}

// fromMinioClientMetadata converts minio metadata to map[string]string
func fromMinioClientMetadata(metadata map[string][]string) map[string]string {
	mm := map[string]string{}
	for k, v := range metadata {
		mm[http.CanonicalHeaderKey(k)] = v[0]
	}
	return mm
}

// toMinioClientMetadata converts metadata to map[string][]string
func toMinioClientMetadata(metadata map[string]string) map[string][]string {
	mm := map[string][]string{}
	for k, v := range metadata {
		mm[http.CanonicalHeaderKey(k)] = []string{v}
	}
	return mm
}

// NewMultipartUpload - upload object in multiple parts
func (l *s3Gateway) NewMultipartUpload(bucket string, object string, metadata map[string]string) (uploadID string, err error) {
	return l.Client.NewMultipartUpload(bucket, object, toMinioClientMetadata(metadata))
}

// CopyObjectPart - copy part of object to other bucket and object
func (l *s3Gateway) CopyObjectPart(srcBucket string, srcObject string, destBucket string, destObject string, uploadID string, partID int, startOffset int64, length int64) (info PartInfo, err error) {
	return PartInfo{}, traceError(NotImplemented{})
}

// fromMinioClientObjectPart - converts minio ObjectPart to PartInfo
func fromMinioClientObjectPart(op minio.ObjectPart) PartInfo {
	return PartInfo{
		Size:         op.Size,
		ETag:         op.ETag,
		LastModified: op.LastModified,
		PartNumber:   op.PartNumber,
	}
}

// PutObjectPart puts a part of object in bucket
func (l *s3Gateway) PutObjectPart(bucket string, object string, uploadID string, partID int, size int64, data io.Reader, md5Hex string, sha256sum string) (PartInfo, error) {
	md5HexBytes, err := hex.DecodeString(md5Hex)
	if err != nil {
		return PartInfo{}, err
	}

	sha256sumBytes, err := hex.DecodeString(sha256sum)
	if err != nil {
		return PartInfo{}, err
	}

	info, err := l.Client.PutObjectPart(bucket, object, uploadID, partID, size, data, md5HexBytes, sha256sumBytes)
	if err != nil {
		return PartInfo{}, err
	}

	return fromMinioClientObjectPart(info), nil
}

// fromMinioClientObjectParts - converts minio ObjectPart to PartInfo
func fromMinioClientObjectParts(parts []minio.ObjectPart) []PartInfo {
	toParts := make([]PartInfo, len(parts))
	for i, part := range parts {
		toParts[i] = fromMinioClientObjectPart(part)
	}
	return toParts
}

// fromMinioClientListPartsInfo converts minio ListObjectPartsResult to ListPartsInfo
func fromMinioClientListPartsInfo(lopr minio.ListObjectPartsResult) ListPartsInfo {
	return ListPartsInfo{
		UploadID:             lopr.UploadID,
		Bucket:               lopr.Bucket,
		Object:               lopr.Key,
		StorageClass:         "",
		PartNumberMarker:     lopr.PartNumberMarker,
		NextPartNumberMarker: lopr.NextPartNumberMarker,
		MaxParts:             lopr.MaxParts,
		IsTruncated:          lopr.IsTruncated,
		EncodingType:         lopr.EncodingType,
		Parts:                fromMinioClientObjectParts(lopr.ObjectParts),
	}
}

// ListObjectParts returns all object parts for specified object in specified bucket
func (l *s3Gateway) ListObjectParts(bucket string, object string, uploadID string, partNumberMarker int, maxParts int) (ListPartsInfo, error) {
	result, err := l.Client.ListObjectParts(bucket, object, uploadID, partNumberMarker, maxParts)
	if err != nil {
		return ListPartsInfo{}, err
	}

	return fromMinioClientListPartsInfo(result), nil
}

// AbortMultipartUpload aborts a ongoing multipart upload
func (l *s3Gateway) AbortMultipartUpload(bucket string, object string, uploadID string) error {
	return l.Client.AbortMultipartUpload(bucket, object, uploadID)
}

// toMinioClientCompletePart converts completePart to minio CompletePart
func toMinioClientCompletePart(part completePart) minio.CompletePart {
	return minio.CompletePart{
		ETag:       part.ETag,
		PartNumber: part.PartNumber,
	}
}

// toMinioClientCompletePart converts []completePart to minio []CompletePart
func toMinioClientCompleteParts(parts []completePart) []minio.CompletePart {
	mparts := make([]minio.CompletePart, len(parts))
	for i, part := range parts {
		mparts[i] = toMinioClientCompletePart(part)
	}
	return mparts
}

// CompleteMultipartUpload completes ongoing multipart upload and finalizes object
func (l *s3Gateway) CompleteMultipartUpload(bucket string, object string, uploadID string, uploadedParts []completePart) (ObjectInfo, error) {
	err := l.Client.CompleteMultipartUpload(bucket, object, uploadID, toMinioClientCompleteParts(uploadedParts))
	if err != nil {
		return ObjectInfo{}, nil
	}

	return l.GetObjectInfo(bucket, object)
}

// SetBucketPolicies - Set policy on bucket
func (l *s3Gateway) SetBucketPolicies(string, []BucketAccessPolicy) error {
	return traceError(NotImplemented{})
}

// GetBucketPolicies - Get policy on bucket
func (l *s3Gateway) GetBucketPolicies(bucket string) ([]BucketAccessPolicy, error) {
	return []BucketAccessPolicy{}, traceError(NotImplemented{})
}

// DeleteBucketPolicies - Delete all policies on bucket
func (l *s3Gateway) DeleteBucketPolicies(string) error {
	return traceError(NotImplemented{})
}
