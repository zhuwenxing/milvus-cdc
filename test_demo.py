import random
import numpy as np
import time
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

def test_multi_process_connect():
    print(f'Start Method: {get_start_method()}')
    conn_list = ["10.100.36.163", "10.100.36.164"]
    lock = Lock()
    with Manager() as manager:
        d_source = manager.dict()
        d_target = manager.dict()
        tasks = []
        for host in conn_list:
            p = Process(target=try_connect, args=(host,d_source, d_target, lock))
            p.start()
            tasks.append(p)
        # for p in tasks:
        #     p.join()
        print("all connected")
        time.sleep(10)
        for p in tasks:
            p.terminate()
        print(f"source: {d_source}")
        print(f"target: {d_target}")

# if __name__ == "__main__":
#     print(f'Start Method: {get_start_method()}')
#     conn_list = ["10.100.36.163", "10.100.36.164"]
#     for host in conn_list:
#         p = Process(target=try_connect, args=(host,))
#         p.start()
#         p.join()
#     print("all connected")
#     time.sleep(10)