# coding: utf-8
"""
    AIS

    AIStore is a scalable object-storage based caching system with Amazon and
    Google Cloud backends.  # noqa: E501

    OpenAPI spec version: 1.1.0
    Contact: test@nvidia.com
    Generated by: https://openapi-generator.tech
"""

# pylint: disable=unused-variable
from __future__ import absolute_import

import unittest

import ais_client
from ais_client.rest import ApiException

import random
import uuid
import os
import logging as log
from .helpers import bytestring, skipPython3, surpressResourceWarning


# pylint: disable=unused-variable
class TestObjectApi(unittest.TestCase):
    """ObjectApi unit test stubs"""
    def setUp(self):
        surpressResourceWarning()

        configuration = ais_client.Configuration()
        configuration.debug = False
        api_client = ais_client.ApiClient(configuration)
        self.object = ais_client.api.object_api.ObjectApi(api_client)
        self.bucket = ais_client.api.bucket_api.BucketApi(api_client)
        self.models = ais_client.models
        self.BUCKET_NAME = os.environ["BUCKET"]
        self.assertTrue(self.BUCKET_NAME, "Environment variable 'BUCKET' not set.")
        self.FILE_SIZE = 128
        self.created_objects = []
        self.created_buckets = []

    def tearDown(self):
        log.info("Cleaning up all created objects in cloud.")
        for object_name in self.created_objects:
            self.object.delete(self.BUCKET_NAME, object_name)
        log.info("Cleaning up all created local buckets.")
        input_params = self.models.InputParameters(self.models.Actions.DESTROY_BCK)
        for bucket_name in self.created_buckets:
            self.bucket.delete(bucket_name, input_params)

    @skipPython3
    def test_object_headers(self):
        object_name, _ = self.__put_random_object()
        headers = self.object.get_with_http_info(self.BUCKET_NAME, object_name)[2]
        self.assertTrue(self.models.Headers.OBJCKSUMTYPE in headers, "ChecksumType not in header for [%s/%s]" % (self.BUCKET_NAME, object_name))
        self.assertTrue(headers[self.models.Headers.OBJCKSUMVAL], "ChecksumVal is None or empty [%s/%s]" % (self.BUCKET_NAME, object_name))

    @skipPython3
    def test_apis_with_get(self):
        """
        Test case for GET, PUT and DELETE object.

        1. Put object
        2. Get object
        3. Delete object
        4. Get object
        :return:
        """
        object_name, input_object = self.__put_random_object()
        log.info("GET object [%s/%s]", self.BUCKET_NAME, object_name)
        output_object = self.object.get(self.BUCKET_NAME, object_name)
        self.assertEqual(
            output_object,
            input_object,
            (
                "Output and input object mismatch. \
                         ExpectedBytes [%d] FetchedBytes [%d]" % (len(input_object), len(output_object))
            )
        )
        log.info("DEL object [%s/%s]", self.BUCKET_NAME, object_name)
        self.object.delete(self.BUCKET_NAME, object_name)
        self.__execute_operation_on_unavailable_object(self.object.get, self.BUCKET_NAME, object_name)

    @skipPython3
    def test_apis_with_range_read(self):
        """
        Test case for GET range, PUT and DELETE object.

        1. Put object
        2. Get range from object
        3. Delete object
        4. Get object
        :return:
        """
        object_name, input_object = self.__put_random_object()
        offset = random.randint(0, self.FILE_SIZE >> 1)
        length = random.randint(1, self.FILE_SIZE >> 2)
        log.info("GET object [%s/%s] offset [%d] length [%d]", self.BUCKET_NAME, object_name, offset, length)
        output_object = self.object.get(self.BUCKET_NAME, object_name, offset=offset, length=length)
        expected_object = input_object[offset:offset + length]
        self.assertEqual(len(output_object), length, ("Length mismatch. Expected [%d] Requested [%d]" % (len(output_object), length)))
        self.assertEqual(
            output_object,
            expected_object,
            (
                "Output and input object mismatch. \
                        ExpectedBytes [%d] FetchedBytes [%d]" % (len(expected_object), len(output_object))
            )
        )
        log.info("DEL object [%s/%s]", self.BUCKET_NAME, object_name)
        self.object.delete(self.BUCKET_NAME, object_name)
        self.__execute_operation_on_unavailable_object(self.object.get, self.BUCKET_NAME, object_name)

    def test_evict(self):
        """
        1. Put object
        2. Evict object
        3. Check if object is in cache
        :return:
        """
        object_name, _ = self.__put_random_object()
        log.info("GET object [%s/%s]", self.BUCKET_NAME, object_name)
        input_params = self.models.InputParameters(self.models.Actions.EVICTOBJECTS)
        log.info("Evict object [%s/%s] InputParameters [%s]", self.BUCKET_NAME, object_name, input_params)
        self.object.delete(self.BUCKET_NAME, object_name, input_parameters=input_params)
        self.__execute_operation_on_unavailable_object(self.object.get_properties, self.BUCKET_NAME, object_name, check_cached=True)

    def test_check_cached(self):
        """
        1. Check if non existent object is in cache
        2. Put object
        3. Check if object created in 2 is in cache
        :return:
        """
        self.__execute_operation_on_unavailable_object(self.object.get_properties, self.BUCKET_NAME, uuid.uuid4().hex, check_cached=True)
        object_name, _ = self.__put_random_object()
        log.info("HEAD object check_cached [%s/%s]", self.BUCKET_NAME, object_name)
        self.object.get_properties(self.BUCKET_NAME, object_name, check_cached=True)

    def test_get_cloud_object_properties(self):
        """
        1. Get properties of a non existent object
        2. Put object
        3. Get properties of object created in 2
        4. Delete object
        :return:
        """
        self.__execute_operation_on_unavailable_object(self.object.get_properties, self.BUCKET_NAME, uuid.uuid4().hex)
        object_name, _ = self.__put_random_object(self.BUCKET_NAME)
        log.info("HEAD cloud object [%s/%s]", self.BUCKET_NAME, object_name)
        headers = self.object.get_properties_with_http_info(self.BUCKET_NAME, object_name)[2]
        self.assertTrue(self.models.Headers.PROVIDER in headers, "PROVIDER not in header for [%s/%s]" % (self.BUCKET_NAME, object_name))

    def test_get_local_object_properties(self):
        """
        1. Get properties of a non existent object
        2. Put object
        3. Get properties of object created in 2
        4. Delete object
        :return:
        """
        bucket_name = self.__create_local_bucket()
        self.__execute_operation_on_unavailable_object(self.object.get_properties, bucket_name, uuid.uuid4().hex)
        object_name, _ = self.__put_random_object(bucket_name)
        log.info("HEAD local object [%s/%s]", bucket_name, object_name)
        headers = self.object.get_properties_with_http_info(bucket_name, object_name)[2]
        self.assertTrue(self.models.Headers.OBJSIZE in headers, "Size not in header for [%s/%s]" % (self.BUCKET_NAME, object_name))
        self.assertTrue(self.models.Headers.OBJVERSION in headers, "Version not in header for [%s/%s]" % (self.BUCKET_NAME, object_name))

    @unittest.skip("This passes with 1 target and not with multiple since the buckets aren't synced yet")
    def test_rename_object(self):
        """
        1. Put object
        2. Get object
        3. Rename object
        4. Get object with old name
        5. Get object with new name
        :return:
        """
        bucket_name = self.__create_local_bucket()
        object_name, expected_object = self.__put_random_object(bucket_name)
        new_object_name = uuid.uuid4().hex
        input_params = self.models.InputParameters(self.models.Actions.RENAME, new_object_name)
        log.info("RENAME object from [%s/%s] to [%s/%s]", bucket_name, object_name, bucket_name, new_object_name)
        self.object.perform_operation(bucket_name, object_name, input_parameters=input_params)
        self.__execute_operation_on_unavailable_object(self.object.get, bucket_name, object_name)
        log.info("GET object [%s/%s]", bucket_name, object_name)
        renamed_object = self.object.get(bucket_name, new_object_name)
        self.assertEqual(
            expected_object, renamed_object, "Renamed object [%d] and actual object [%d] mismatch." % (len(renamed_object), len(expected_object))
        )

    def __execute_operation_on_unavailable_object(self, operation, bucket_name, object_name, **kwargs):
        log.info("[%s] on unavailable object [%s/%s]", operation.__name__, bucket_name, object_name)
        with self.assertRaises(ApiException) as contextManager:
            operation(bucket_name, object_name, **kwargs)
        exception = contextManager.exception
        self.assertEqual(exception.status, 404)

    def __put_random_object(self, bucket_name=None):
        bucket_name = bucket_name if bucket_name else self.BUCKET_NAME
        object_name = uuid.uuid4().hex
        input_object = bytestring(os.urandom(self.FILE_SIZE))
        log.info("PUT object [%s/%s] size [%d]", bucket_name, object_name, self.FILE_SIZE)
        self.object.put(bucket_name, object_name, body=input_object)
        if bucket_name == self.BUCKET_NAME:
            self.created_objects.append(object_name)
        return object_name, input_object

    def __create_local_bucket(self):
        bucket_name = uuid.uuid4().hex
        input_params = self.models.InputParameters(self.models.Actions.CREATE_BCK)
        log.info("Create local bucket [%s]", bucket_name)
        self.bucket.perform_operation(bucket_name, input_params)
        self.created_buckets.append(bucket_name)
        return bucket_name


if __name__ == '__main__':
    log.basicConfig(format='[%(levelname)s]:%(message)s', level=log.DEBUG)
    unittest.main()
