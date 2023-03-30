import pytest
from common import common_func as cf
from utils.util_log import test_log as log
from api.milvus_cdc import MilvusCdcClient



prefix = "cdc_e2e"
client = MilvusCdcClient('http://localhost:8444')

class TestE2E():
    """ Test Milvus CDC end to end """
    def test_milvus_cdc_default(self, downstream_host, downstream_port):
        '''
        target: test cdc default
        method: create task with default params
        expected: create successfully
        '''
        collection_name = cf.gen_unique_str(prefix)
        request_data = {
            "milvus_connect_param": {
                "host": downstream_host,
                "port": downstream_port,
                "username": "",
                "password": "",
                "enable_tls": False,
                "ignore_partition": True,
                "connect_timeout": 10
            },
            "collection_infos": [
                {
                    "name": collection_name
                }
            ]
        }
        # create task
        rsp, result = client.create_task(request_data)
        assert result
        log.info(f"create task response: {rsp}")
        task_id = rsp['task_id']
        # get the task
        rsp, result = client.get_task(task_id)
        assert result
        log.info(f"get task {task_id} response: {rsp}")
        # create collection in upstream and do DDL and DML operations
        




