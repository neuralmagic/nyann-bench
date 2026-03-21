just deploy sharegpt-load http://tms-wide-ep-inference-gateway-istio.vllm.svc.cluster.local/v1 \
  '{"load":{"concurrency":1900,"rampup":"3m","duration":"3600s"},"workload":{"type":"corpus","corpus_path":"/mnt/lustre/tms/corpus/sharegpt.txt","isl":100,"osl":1500,"turns":1}}' \
  16 vllm arm64 lustre &

just deploy poker-eval http://tms-wide-ep-inference-gateway-istio.vllm.svc.cluster.local/v1 \
  '{"load":{"concurrency":64,"duration":"3600s"},"warmup":{"concurrency":16,"duration":"60s"},"workload":{"type":"gsm8k","gsm8k_path":"/mnt/lustre/tms/gsm8k_test.jsonl","gsm8k_train_path":"/mnt/lustre/tms/gsm8k_train.jsonl"}}' \
  1 vllm arm64 lustre &
