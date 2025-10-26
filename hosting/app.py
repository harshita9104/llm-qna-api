from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from transformers import AutoTokenizer, AutoModelForCausalLM, GenerationConfig
import torch
from contextlib import asynccontextmanager

MODEL_NAME = "HuggingFaceTB/SmolLM2-135M-Instruct"

# Global variables for model components
tokenizer = None
model = None
device = None
gen_config = None

@asynccontextmanager
async def lifespan(app: FastAPI):
    # Startup
    global tokenizer, model, device, gen_config
    device = "cpu"
    print("Loading model... please wait 1–2 minutes.")
    tokenizer = AutoTokenizer.from_pretrained(MODEL_NAME)
    model = AutoModelForCausalLM.from_pretrained(MODEL_NAME)
    model.to(device)
    gen_config = GenerationConfig(
        max_new_tokens=150,
        do_sample=True,
        temperature=0.7,
        top_p=0.9,
    )
    print("✅ Model loaded successfully!")
    yield
    # Shutdown (cleanup if needed)
    print("Shutting down...")

app = FastAPI(title="SmolLM2 Hosting Service", lifespan=lifespan)

class ChatRequest(BaseModel):
    chat_id: str
    system_prompt: str | None = ""
    user_prompt: str


class BatchedRequest(BaseModel):
    queries: list[ChatRequest]


@app.post("/generate_batch")
def generate_batch(req: BatchedRequest):
    try:
        if not req.queries:
            raise HTTPException(status_code=400, detail="queries array required and must be non-empty")
        
        # Build responses for each query (keeping order)
        responses = []
        valid_prompts = []
        valid_indices = []
        
        # First pass: separate valid and invalid queries
        for i, q in enumerate(req.queries):
            if not q.chat_id or q.chat_id.strip() == "":
                responses.append({"chat_id": q.chat_id or f"query_{i}", "error": "chat_id is required and cannot be empty"})
            elif not q.user_prompt or q.user_prompt.strip() == "":
                responses.append({"chat_id": q.chat_id, "error": "user_prompt is required and cannot be empty"})
            else:
                # Valid query - will be processed
                valid_prompts.append((q.system_prompt or "") + "\n" + q.user_prompt)
                valid_indices.append(len(responses))  # Remember where to put the result
                responses.append({"placeholder": True, "chat_id": q.chat_id})  # Temporary
        
        # Process valid queries in batch
        if valid_prompts:
            inputs = tokenizer(valid_prompts, return_tensors="pt", padding=True, truncation=True).to(device)
            outputs = model.generate(**inputs, **gen_config.__dict__)
            
            # Replace placeholders with real responses
            for j, output in enumerate(outputs):
                text = tokenizer.decode(output, skip_special_tokens=True)
                response_index = valid_indices[j]
                responses[response_index] = {"chat_id": responses[response_index]["chat_id"], "response": text}
        
        return {"responses": responses}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))

@app.post("/chat")
def chat(req: ChatRequest):
    try:
        if not req.chat_id or not req.user_prompt:
            raise HTTPException(status_code=400, detail="chat_id and user_prompt are required")
        prompt = (req.system_prompt or "") + "\n" + req.user_prompt
        inputs = tokenizer(prompt, return_tensors="pt").to(device)
        outputs = model.generate(**inputs, **gen_config.__dict__)
        text = tokenizer.decode(outputs[0], skip_special_tokens=True)
        return {"chat_id": req.chat_id, "response": text}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
