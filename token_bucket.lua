local key = KEYS[1]
local now_ms = tonumber(ARGV[1])
local rps = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])

local data = redis.call("HMGET", key, "tokens", "ts")
local tokens = tonumber(data[1]) or burst
local ts = tonumber(data[2]) or now_ms

-- refill
local elapsed = (now_ms - ts) / 1000.0
tokens = math.min(burst, tokens + elapsed * rps)

local allowed = 0
if tokens >= 1.0 then
	tokens = tokens - 1.0
	allowed = 1
end

redis.call("HMSET", key, "tokens", tokens, "ts", now_ms)
redis.call("EXPIRE", key, 600)

return { allowed, tokens }
