/**
 * The goals the empty goal box types as its placeholder, one after another, in this order. Each
 * states the number to move and which way, what to beat and what counts as a win, so that they
 * teach a goal agents can check. They come from different fields so that none reads as the only kind.
 */
export const GOAL_EXAMPLES = [
  'Does Muon reach val loss 3.28 in fewer tokens than AdamW for a 125M GPT on FineWeb? Same batch, schedule and a 16-trial LR sweep each; a win must beat the spread of 3 seeds.',
  'Raise the lossless compression ratio of our KITTI lidar scans 20% past Draco at its default settings; decoding must be bit-exact, and nothing that only fits these files counts.',
  'Cut the median latency of kernels/attn.cu by 15% on one H100 at sequence length 4096, outputs within 1e-3 of the reference; bench.py and tests/ stay as they are.',
  'Lift CIFAR-100 top-1 of ResNet-18 above the 76.1% train.py reaches today, in 30 minutes on one RTX 4090, mean of 3 seeds, with the test set never used for tuning.',
  'Find the LoRA rank and learning rate that maximise GSM8K exact match for Llama-3-8B, training on the train split only, at most one GPU-hour per trial.',
  'Make PPO reach return 6000 on MuJoCo Humanoid in fewer environment steps than the repository baseline, with the same network and 5 seeds per arm, compared on the median.',
  'Lower the word error rate of Whisper-small with 8-bit weights on LibriSpeech test-other to within 0.3 points of fp16, decoding settings fixed.',
  'Find the batch size and precision with the most images per second for ResNet-50 inference on one L40S, with top-1 matching fp32 on 99% of a fixed set of 5,000 images.',
  'Raise ProteinGym Spearman of our ESM-2 650M scorer past its current 0.41 without training on any assay of the test folds.',
  'Does a cosine schedule beat linear decay for ViT-B on ImageNet-1k at an equal 90 epochs, each with the same peak LR sweep? Judged on top-1, 2 seeds each.',
  'Find the KV-cache eviction policy that keeps Llama-3-8B within 1 point of its full-cache LongBench score at a quarter of the memory.',
]
