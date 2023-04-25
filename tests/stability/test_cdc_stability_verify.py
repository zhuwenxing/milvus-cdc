import random
import numpy as np
import time
import uuid
from datetime import datetime
import argparse
from pymilvus import (
    connections, list_collections,
    FieldSchema, CollectionSchema, DataType,
    Collection
)
from multiprocessing import Process, Lock, Manager
from multiprocessing import set_start_method
from multiprocessing import get_start_method
from utils.util_log import test_log as log
from api.milvus_cdc import MilvusCdcClient
from pymilvus import (
    connections, list_collections,
    Collection
)
from base.checker import (
    InsertEntitiesCollectionChecker,
    DeleteEntitiesCollectionChecker,
)
from base.client_base import TestBase

client = MilvusCdcClient('http://localhost:8444')
TIMEOUT = 120

def try_connect(host, d_source, d_target, lock):
    while True:
        from pymilvus import connections
        data = datetime.now().strftime('%Y_%m_%d_%H_%M_%S_%f')
        if host == "10.100.36.163": # source
            d_source[data] = data
        if host == "10.100.36.164": # target
            d_target[data] = data
        connections.connect(host=host, port="19530")
        print(f"connect to {host} success")
        print(f"all collections: {list_collections()}")
        print(f"source: {d_source}")
        print(f"target: {d_target}")
        time.sleep(2)

def get_count_by_query_in_downstream(host, port, c_name_list, source_entities_num_infos):
    connections.connect(host=host, port=port)
    log.info(f"connect to upstream {host}:{port} success")
    log.info(f"list connections: {connections.list_connections()}")
    log.info(f"list collections: {list_collections()}")
    delay_cnt = 0
    for c_name in c_name_list:
        t0 = time.time()
        timeout = 60
        while True and time.time() - t0 < timeout:
            if c_name in list_collections():
                log.info(f"collection {c_name} is created")
                break
            time.sleep(1)
            if time.time() - t0 > timeout:
                raise Exception(f"Timeout waiting for collection {c_name} to be created")    
        col = Collection(name=c_name)
        index_infos = [index.to_dict() for index in col.indexes]
        if len(index_infos) == 0:
            col.create_index(field_name="float_vector",
                            index_params={"index_type": "IVF_FLAT", "metric_type": "L2", "params": {"nlist": 128}})
        col.load()
    while True:
        for c_name in c_name_list:
            col = Collection(name=c_name)
            count_by_query_upstream = source_entities_num_infos.get(c_name, 0)
            t0 = time.time()
            while True and time.time() - t0 < timeout:
                count_by_query_downstream = len(col.query(expr="int64 >= 0", output_fields=["int64"]))
                if count_by_query_downstream >= count_by_query_upstream:
                    log.info(f"sync done: count_by_query_downstream: {count_by_query_downstream} >= count_by_query_upstream: {count_by_query_upstream}")
                    break
                time.sleep(1)
                if time.time() - t0 > timeout:
                    delay_cnt += 1
                    log.info(f"delay_cnt: {delay_cnt}")
                    break
            log.info(f"count_by_query_upstream: {count_by_query_downstream}")
        if delay_cnt > 10:
            assert False
        time.sleep(60)


def get_count_by_query_in_upstream(host, port, c_name_list, source_entities_num_infos):
    connections.connect(host=host, port=port)
    log.info(f"connect to upstream {host}:{port} success")
    log.info(f"list connections: {connections.list_connections()}")
    log.info(f"list collections: {list_collections()}")   
    for c_name in c_name_list:
        t0 = time.time()
        timeout = 60
        while True and time.time() - t0 < timeout:
            if c_name in list_collections():
                log.info(f"collection {c_name} is created")
                break
            if time.time() - t0 > timeout:
                raise Exception(f"Timeout waiting for collection {c_name} to be created")    
        col = Collection(name=c_name)
        index_infos = [index.to_dict() for index in col.indexes]
        if len(index_infos) == 0:
            col.create_index(field_name="float_vector",
                            index_params={"index_type": "IVF_FLAT", "metric_type": "L2", "params": {"nlist": 128}})
        col.load()
    while True:
        for c_name in c_name_list:
            col = Collection(name=c_name)
            count_by_query_upstream = len(col.query(expr="int64 >= 0", output_fields=["int64"]))
            source_entities_num_infos[c_name] = count_by_query_upstream
            log.info(f"count_by_query_upstream: {count_by_query_upstream}")
        time.sleep(60)




def test_multi_process_connect(upstream_host, upstream_port, downstream_host, downstream_port, duration_time):
    print(f'Start Method: {get_start_method()}')
    all_collections = []
    rsp,  result = client.list_tasks()
    assert result
    for task in rsp["tasks"]:
        c_infos = task["collection_infos"]
        for c_info in c_infos:
            all_collections.append(c_info["name"])

    collections_to_sync = all_collections
    log.info(f"collections_to_sync: {collections_to_sync}")
    
    with Manager() as manager:
        source_entities_num_infos = manager.dict()
        p_upstream = Process(target=get_count_by_query_in_upstream, args=(upstream_host, upstream_port, collections_to_sync, source_entities_num_infos))
        p_upstream.start()
        p_downstream = Process(target=get_count_by_query_in_downstream, args=(downstream_host, downstream_port, collections_to_sync, source_entities_num_infos))
        p_downstream.start()

        time.sleep(duration_time+360)
        p_upstream.terminate()
        p_downstream.terminate()
    log.info("test finished")
