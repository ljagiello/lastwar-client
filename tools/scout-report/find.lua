-- usage: lua5.3 find.lua <dir> <id>...
-- Charge chaque table de données du jeu et affiche celles dont T.data contient un des ids,
-- avec les colonnes de la ligne trouvée (noms tirés de T.index).
local dir = arg[1]
local ids = {}
for i = 2, #arg do ids[tonumber(arg[i])] = true end

local function cell(T, v, linked)
  if linked and type(v) == "number" and T.vExt then return tostring(T.vExt[v]) end
  if type(v) == "table" then
    local t = {}
    for _, x in ipairs(v) do t[#t + 1] = tostring(x) end
    return "{" .. table.concat(t, ",") .. "}"
  end
  return tostring(v)
end

for name in io.popen("ls " .. dir):lines() do
  local f, err = loadfile(dir .. "/" .. name, "b")
  if not f then io.stderr:write(name .. ": " .. tostring(err) .. "\n") end
  if f then
    local ok, T = pcall(f)
    if ok and type(T) == "table" and type(T.data) == "table" then
      for id in pairs(ids) do
        local row = T.data[id]
        if row then
          local cols = {}
          for col, d in pairs(T.index or {}) do
            cols[#cols + 1] = { d[1], col .. "=" .. cell(T, row[d[1]], d[3]) }
          end
          table.sort(cols, function(a, b) return a[1] < b[1] end)
          local out = {}
          for _, c in ipairs(cols) do out[#out + 1] = c[2] end
          print(name, id, table.concat(out, " | "))
        end
      end
    end
  end
end
