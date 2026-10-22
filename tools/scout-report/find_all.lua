-- usage: lua5.3 find_all.lua <dir> <id>...
-- Affiche seulement les tables dont T.data contient TOUS les ids, puis chaque ligne trouvée
-- (colonnes non vides seulement, valeurs tronquées à 120 caractères).
local dir = arg[1]
local ids = {}
for i = 2, #arg do ids[#ids + 1] = tonumber(arg[i]) end

local function cell(T, v, linked)
  if linked and type(v) == "number" and T.vExt then v = T.vExt[v] end
  if type(v) == "table" then
    local t = {}
    for k, x in pairs(v) do t[#t + 1] = tostring(k) .. ":" .. tostring(x) end
    v = "{" .. table.concat(t, ",") .. "}"
  end
  v = tostring(v)
  if #v > 120 then v = v:sub(1, 120) .. "…" end
  return v
end

for name in io.popen("ls " .. dir):lines() do
  local f = loadfile(dir .. "/" .. name, "b")
  if f then
    local ok, T = pcall(f)
    if ok and type(T) == "table" and type(T.data) == "table" then
      local all = true
      for _, id in ipairs(ids) do
        if T.data[id] == nil then all = false break end
      end
      if all then
        print("== " .. name)
        for _, id in ipairs(ids) do
          local row = T.data[id]
          local cols = {}
          for col, d in pairs(T.index or {}) do
            local v = row[d[1]]
            if v ~= nil and v ~= "" then
              cols[#cols + 1] = { d[1], col .. "=" .. cell(T, v, d[3]) }
            end
          end
          table.sort(cols, function(a, b) return a[1] < b[1] end)
          local out = {}
          for _, c in ipairs(cols) do out[#out + 1] = c[2] end
          print(id, table.concat(out, " | "))
        end
      end
    end
  end
end
