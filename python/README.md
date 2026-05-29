# Python - ML Model Training

Python is used for model training only. Inference happens in Go for hot-path performance.

## Setup

```bash
cd python
python3 -m venv venv
source venv/bin/activate
pip install -r requirements.txt
```

## Directories

- `fraud/` - fraud detection model training (bot classification, anomaly detection)
- `optimise/` - bid optimisation models (win-rate prediction, bid shading curves)
- `contextual/` - page content classification (IAB category prediction from text)

## Workflow

1. Train model in Jupyter notebook
2. Export as ONNX or simple rules/lookup table
3. Go service loads the exported artifact
4. No Python in the serving path
