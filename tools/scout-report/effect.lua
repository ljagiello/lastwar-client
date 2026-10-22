-- usage: lua5.3 effect.lua <dir> <id>...  -- rows of lw_effect_number only, compact
local T = assert(loadfile(arg[1] .. "/lw_effect_number", "b"))()
local keep = {"name","desc","type","target_unit_type","target_army_type","basic_attributes","icon","small_icon","season_name"}
for i = 2, #arg do
  local id = tonumber(arg[i]); local row = T.data[id]
  if not row then print(id, "ABSENT") else
    local out = {}
    for _, c in ipairs(keep) do
      local d = T.index[c]
      if d then local v = row[d[1]]
        if d[3] and type(v) == "number" and T.vExt then v = T.vExt[v] end
        if v ~= nil and v ~= "" then out[#out+1] = c .. "=" .. tostring(v):gsub("Assets/Main/Sprites/UI/UIPveBattleBuff/", "") end
      end
    end
    print(id, table.concat(out, " | "))
  end
end
