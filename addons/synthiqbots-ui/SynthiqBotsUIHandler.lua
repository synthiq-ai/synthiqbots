-- TIMER --

SynthiqBotsUI:SetScript("OnUpdate", function(pSelf, pElapsed)
	if(SynthiqBotsUI.auto.invite) then SynthiqBotsUI.timer.invite.elapsed = SynthiqBotsUI.timer.invite.elapsed + pElapsed end
	if(SynthiqBotsUI.auto.talent) then SynthiqBotsUI.timer.talent.elapsed = SynthiqBotsUI.timer.talent.elapsed + pElapsed end
	if(SynthiqBotsUI.auto.stats) then SynthiqBotsUI.timer.stats.elapsed = SynthiqBotsUI.timer.stats.elapsed + pElapsed end
	if(SynthiqBotsUI.auto.sort) then SynthiqBotsUI.timer.sort.elapsed = SynthiqBotsUI.timer.sort.elapsed + pElapsed end
	
	if(SynthiqBotsUI.auto.stats and SynthiqBotsUI.timer.stats.elapsed >= SynthiqBotsUI.timer.stats.interval) then
		for i = 1, GetNumPartyMembers() do SendChatMessage("stats", "WHISPER", nil, UnitName("party" .. i)) end
		SynthiqBotsUI.timer.stats.elapsed = 0
	end
	
	if(SynthiqBotsUI.auto.talent and SynthiqBotsUI.timer.talent.elapsed >= SynthiqBotsUI.timer.talent.interval) then
		SynthiqBotsUI.talent.setTalents()
		SynthiqBotsUI.timer.talent.elapsed = 0
		SynthiqBotsUI.auto.talent = false
	end
	
	if(SynthiqBotsUI.auto.invite and SynthiqBotsUI.timer.invite.elapsed >= SynthiqBotsUI.timer.invite.interval) then
		local tTable = SynthiqBotsUI.index[SynthiqBotsUI.timer.invite.roster]
		
		if(SynthiqBotsUI.timer.invite.needs == 0 or SynthiqBotsUI.timer.invite.index > table.getn(tTable)) then
			if(SynthiqBotsUI.timer.invite.roster == "raidus") then
				SynthiqBotsUI.timer.sort.elapsed = 0
				SynthiqBotsUI.timer.sort.index = 1
				SynthiqBotsUI.timer.sort.needs = 0
				SynthiqBotsUI.auto.sort = true
			end
			
			SynthiqBotsUI.timer.invite.elapsed = 0
			SynthiqBotsUI.timer.invite.roster = ""
			SynthiqBotsUI.timer.invite.index = 1
			SynthiqBotsUI.timer.invite.needs = 0
			SynthiqBotsUI.auto.invite = false
			return
		end
		
		if(SynthiqBotsUI.isMember(tTable[SynthiqBotsUI.timer.invite.index]) == false) then
			SendChatMessage(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.inviting, "NAME", tTable[SynthiqBotsUI.timer.invite.index]), "SAY")
			SendChatMessage(".playerbot bot add " .. tTable[SynthiqBotsUI.timer.invite.index], "SAY")
			SynthiqBotsUI.timer.invite.needs = SynthiqBotsUI.timer.invite.needs - 1
		end
		
		SynthiqBotsUI.timer.invite.index = SynthiqBotsUI.timer.invite.index + 1
		SynthiqBotsUI.timer.invite.elapsed = 0
	end
	
	if(SynthiqBotsUI.auto.sort and SynthiqBotsUI.timer.sort.elapsed >= SynthiqBotsUI.timer.sort.interval) then
		SynthiqBotsUI.timer.sort.index = SynthiqBotsUI.raidus.doRaidSort(SynthiqBotsUI.timer.sort.index)
		
		if(SynthiqBotsUI.timer.sort.index == nil) then
			SynthiqBotsUI.timer.sort.index = SynthiqBotsUI.raidus.doRaidSortCheck()
		end
		
		if(SynthiqBotsUI.timer.sort.index == nil) then
			SendChatMessage("Ready for Raid now.", "SAY")
			SynthiqBotsUI.timer.sort.elapsed = 0
			SynthiqBotsUI.timer.sort.index = 1
			SynthiqBotsUI.timer.sort.needs = 0
			SynthiqBotsUI.auto.sort = false
			return
		end
		
		SynthiqBotsUI.timer.sort.elapsed = 0
	end
end)

-- AUTO-CHAT GATE --
-- Gates outbound chat sends from the auto-handler (CHAT_MSG_WHISPER block).
-- When SynthiqBotsUI.auto.botCommands is false, the addon stops auto-replying to
-- bot whispers (co ?, nc ?, summon, stats, ...). UI button clicks still send.
SynthiqBotsUI.sendBotChat = function(pMessage, pType, pLang, pTarget)
	if(SynthiqBotsUI.auto.botCommands == false) then return end
	SendChatMessage(pMessage, pType, pLang, pTarget)
end

-- HANDLER --

SynthiqBotsUI:SetScript("OnEvent", function()
	if(event == "PLAYER_LOGOUT") then
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.frames["MultiBar"])
		SynthiqBotsUISave["MultiBarPoint"] = tX .. ", " .. tY
		
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.inventory)
		SynthiqBotsUISave["InventoryPoint"] = tX .. ", " .. tY
		
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.spellbook)
		SynthiqBotsUISave["SpellbookPoint"] = tX .. ", " .. tY
		
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.itemus)
		SynthiqBotsUISave["ItemusPoint"] = tX .. ", " .. tY
		
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.iconos)
		SynthiqBotsUISave["IconosPoint"] = tX .. ", " .. tY
		
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.stats)
		SynthiqBotsUISave["StatsPoint"] = tX .. ", " .. tY
		
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.reward)
		SynthiqBotsUISave["RewardPoint"] = tX .. ", " .. tY
		
		local tX, tY = SynthiqBotsUI.toPoint(SynthiqBotsUI.talent)
		SynthiqBotsUISave["TalentPoint"] = tX .. ", " .. tY
		
		local tPortal = SynthiqBotsUI.frames["MultiBar"].frames["Masters"].frames["Portal"]
		SynthiqBotsUISave["MemoryGem1"] =  SynthiqBotsUI.SavePortal(tPortal.buttons["Red"])
		SynthiqBotsUISave["MemoryGem2"] =  SynthiqBotsUI.SavePortal(tPortal.buttons["Green"])
		SynthiqBotsUISave["MemoryGem3"] =  SynthiqBotsUI.SavePortal(tPortal.buttons["Blue"])
		
		local tValue = SynthiqBotsUI.doSplit(SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Attack"].texture, "\\")[5]
		tValue = string.sub(tValue, 1, string.len(tValue) - 4)
		SynthiqBotsUISave["AttackButton"] = tValue
		
		local tValue = SynthiqBotsUI.doSplit(SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Flee"].texture, "\\")[5]
		tValue = string.sub(tValue, 1, string.len(tValue) - 4)
		SynthiqBotsUISave["FleeButton"] = tValue
		
		SynthiqBotsUISave["AutoRelease"] = SynthiqBotsUI.IF(SynthiqBotsUI.auto.release, "true", "false")
		SynthiqBotsUISave["AutoChatCommands"] = SynthiqBotsUI.IF(SynthiqBotsUI.auto.botCommands, "true", "false")
		SynthiqBotsUISave["NecroNet"] = SynthiqBotsUI.IF(SynthiqBotsUI.necronet.state, "true", "false")
		SynthiqBotsUISave["Reward"] = SynthiqBotsUI.IF(SynthiqBotsUI.reward.state, "true", "false")
		
		SynthiqBotsUISave["Masters"] = SynthiqBotsUI.IF(SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Masters"].state, "true", "false")
		SynthiqBotsUISave["Creator"] = SynthiqBotsUI.IF(SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Creator"].state, "true", "false")
		SynthiqBotsUISave["Beast"] = SynthiqBotsUI.IF(SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Beast"].state, "true", "false")
		SynthiqBotsUISave["Expand"] = SynthiqBotsUI.IF(SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Expand"].state, "true", "false")
		SynthiqBotsUISave["RTSC"] = SynthiqBotsUI.IF(SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["RTSC"].state, "true", "false")
		
		return
	end
	
	-- ADDON:LOADED --
	
	if(event == "ADDON_LOADED" and arg1 == "SynthiqBotsUI") then
		if(SynthiqBotsUISave["MultiBarPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["MultiBarPoint"], ", ")
			SynthiqBotsUI.frames["MultiBar"].setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["InventoryPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["InventoryPoint"], ", ")
			SynthiqBotsUI.inventory.setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["SpellbookPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["SpellbookPoint"], ", ")
			SynthiqBotsUI.spellbook.setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["ItemusPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["ItemusPoint"], ", ")
			SynthiqBotsUI.itemus.setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["IconosPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["IconosPoint"], ", ")
			SynthiqBotsUI.iconos.setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["StatsPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["StatsPoint"], ", ")
			SynthiqBotsUI.stats.setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["RewardPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["RewardPoint"], ", ")
			SynthiqBotsUI.reward.setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["TalentPoint"] ~= nil) then
			local tPoint = SynthiqBotsUI.doSplit(SynthiqBotsUISave["TalentPoint"], ", ")
			SynthiqBotsUI.talent.setPoint(tonumber(tPoint[1]), tonumber(tPoint[2]))
		end
		
		if(SynthiqBotsUISave["MemoryGem1"] ~= nil) then
			local tGem = SynthiqBotsUI.frames["MultiBar"].frames["Masters"].frames["Portal"].buttons["Red"]
			SynthiqBotsUI.LoadPortal(tGem, SynthiqBotsUISave["MemoryGem1"])
		end
		
		if(SynthiqBotsUISave["MemoryGem2"] ~= nil) then
			local tGem = SynthiqBotsUI.frames["MultiBar"].frames["Masters"].frames["Portal"].buttons["Green"]
			SynthiqBotsUI.LoadPortal(tGem, SynthiqBotsUISave["MemoryGem2"])
		end
		
		if(SynthiqBotsUISave["MemoryGem3"] ~= nil) then
			local tGem = SynthiqBotsUI.frames["MultiBar"].frames["Masters"].frames["Portal"].buttons["Blue"]
			SynthiqBotsUI.LoadPortal(tGem, SynthiqBotsUISave["MemoryGem3"])
		end
		
		if(SynthiqBotsUISave["AttackButton"] ~= nil) then
			if(SynthiqBotsUISave["AttackButton"] == "attack") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Attack"].buttons["Attack"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["AttackButton"] == "attack_ranged") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Attack"].buttons["Ranged"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["AttackButton"] == "attack_melee") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Attack"].buttons["Melee"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["AttackButton"] == "attack_healer") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Attack"].buttons["Healer"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["AttackButton"] == "attack_dps") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Attack"].buttons["Dps"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["AttackButton"] == "attack_tank") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Attack"].buttons["Tank"]
				tButton.doRight(tButton)
			end
		end
		
		if(SynthiqBotsUISave["FleeButton"] ~= nil) then
			if(SynthiqBotsUISave["FleeButton"] == "flee") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Flee"].buttons["Flee"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["FleeButton"] == "flee_ranged") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Flee"].buttons["Ranged"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["FleeButton"] == "flee_melee") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Flee"].buttons["Melee"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["FleeButton"] == "flee_healer") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Flee"].buttons["Healer"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["FleeButton"] == "flee_dps") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Flee"].buttons["Dps"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["FleeButton"] == "flee_tank") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Flee"].buttons["Tank"]
				tButton.doRight(tButton)
				
			elseif(SynthiqBotsUISave["FleeButton"] == "flee_target") then
				local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Flee"].buttons["Target"]
				tButton.doRight(tButton)
			end
		end
		
		if(SynthiqBotsUISave["AutoRelease"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Release"]

			if(SynthiqBotsUISave["AutoRelease"] == "true")
			then tButton.setDisable()
			else tButton.setEnable()
			end

			tButton.doLeft(tButton)
		end

		if(SynthiqBotsUISave["AutoChatCommands"] ~= nil) then
			SynthiqBotsUI.auto.botCommands = (SynthiqBotsUISave["AutoChatCommands"] == "true")
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["AutoChat"]
			if(tButton ~= nil) then
				if(SynthiqBotsUI.auto.botCommands) then tButton.setEnable() else tButton.setDisable() end
			end
		end
		
		if(SynthiqBotsUISave["NecroNet"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Masters"].buttons["NecroNet"]
			
			if(SynthiqBotsUISave["NecroNet"] == "true")
			then tButton.setDisable()
			else tButton.setEnable()
			end
			
			tButton.doLeft(tButton)
		end
		
		if(SynthiqBotsUISave["Reward"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Reward"]
			
			if(SynthiqBotsUISave["Reward"] == "true")
			then tButton.setDisable()
			else tButton.setEnable()
			end
			
			tButton.doLeft(tButton)
		end
		
		if(SynthiqBotsUISave["Masters"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Masters"]
			
			if(SynthiqBotsUISave["Masters"] == "true") then
				SynthiqBotsUI.GM = true
				tButton.setDisable()
				tButton.doLeft(tButton)
			end
		end
		
		if(SynthiqBotsUISave["Creator"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Creator"]
			
			if(SynthiqBotsUISave["Creator"] == "true") then
				tButton.setDisable()
				tButton.doLeft(tButton)
			end
		end
		
		if(SynthiqBotsUISave["Beast"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Beast"]
			
			if(SynthiqBotsUISave["Beast"] == "true") then
				tButton.setDisable()
				tButton.doLeft(tButton)
			end
		end
		
		if(SynthiqBotsUISave["Expand"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Expand"]
			
			if(SynthiqBotsUISave["Expand"] == "true") then
				tButton.setDisable()
				tButton.doLeft(tButton)
			end
		end
		
		if(SynthiqBotsUISave["RTSC"] ~= nil) then
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["RTSC"]
			
			if(SynthiqBotsUISave["RTSC"] == "true") then
				SynthiqBotsUI.frames["MultiBar"].setPoint(SynthiqBotsUI.frames["MultiBar"].x, SynthiqBotsUI.frames["MultiBar"].y - 34)
				tButton.setDisable()
				tButton.doLeft(tButton)
			end
		end
		
		return
	end
	
	-- PLAYER:ENTERING --
	
	if(event == "PLAYER_ENTERING_WORLD") then
		SendChatMessage(".account", "SAY")
		
		if(SynthiqBotsUI.init == nil) then
			SendChatMessage(".playerbot bot list", "SAY")
			SynthiqBotsUI.init = true
			return
		end
		
		return
	end
	
	-- CHAT:SYSTEM --
	
	if(event == "CHAT_MSG_SYSTEM") then
		if(SynthiqBotsUI.isInside(arg1, "Accountlevel", "account level", "niveau de compte", "等级")) then
			local tLevel = tonumber(SynthiqBotsUI.doSplit(arg1, ": ")[2])
			if(tLevel ~= nil) then SynthiqBotsUI.GM = tLevel > 1 end
			SynthiqBotsUI.RaidPool("player")
		end
		
		if(SynthiqBotsUI.isInside(arg1, "Possible strategies")) then
			local tStrategies = SynthiqBotsUI.doSplit(arg1, ", ")
			SendChatMessage("=== STRATEGIES ===", "SAY")
			for i = 1, table.getn(tStrategies) do SendChatMessage(i .. " : " .. tStrategies[i], "SAY") end
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "Whisper any of")) then
			local tCommands = SynthiqBotsUI.doSplit(arg1, ", ")
			SendChatMessage("=== WHISPER-COMMANDS ===", "SAY")
			for i = 1, table.getn(tCommands) do SendChatMessage(i .. " : " .. tCommands[i], "SAY") end
			return
		end
		
		if(SynthiqBotsUI.auto.release == true) then
			if(SynthiqBotsUI.isInside(arg1, "已经死亡")) then
				SendChatMessage("release", "WHISPER", nil, SynthiqBotsUI.doReplace(arg1, "已经死亡。", ""))
				return
			end
			
			if(SynthiqBotsUI.isInside(arg1, "ist tot", "has dies", "has died")) then
				SendChatMessage("release", "WHISPER", nil, SynthiqBotsUI.doSplit(arg1, " ")[1])
				return
			end
		end
		
		if(string.sub(arg1, 1, 12) == "Bot roster: ") then
			local tLocClass, tClass, tLocRace, tRace, tSex, tName = GetPlayerInfoByGUID(UnitGUID("player"))
			tClass = SynthiqBotsUI.toClass(tClass)
			
			local tPlayer = SynthiqBotsUI.addSelf(tClass, tName).setDisable()
			tPlayer.class = tClass
			tPlayer.name = tName
			
			tPlayer.doLeft = function(pButton)
				SendChatMessage(".playerbot bot self", "SAY")
				SynthiqBotsUI.OnOffSwitch(pButton)
			end
			
			-- PLAYERBOTS --
			
			local tTable = SynthiqBotsUI.doSplit(string.sub(arg1, 13), ", ")
			
			for key, value in pairs(tTable) do
				if(value == "") then break end
				local tBot = SynthiqBotsUI.doSplit(value, " ")
				local tName = string.sub(tBot[1], 2)
				local tClass = SynthiqBotsUI.toClass(tBot[2])
				local tOnline = string.sub(tBot[1], 1, 1)
				
				local tPlayer = SynthiqBotsUI.addPlayer(tClass, tName).setDisable()
				
				tPlayer.doRight = function(pButton)
					if(pButton.state == false) then return end
					SendChatMessage(".playerbot bot remove " .. pButton.name, "SAY")
					if(pButton.parent.frames[pButton.name] ~= nil) then pButton.parent.frames[pButton.name]:Hide() end
					pButton.setDisable()
				end
				
				tPlayer.doLeft = function(pButton)
					if(pButton.state) then
						if(pButton.parent.frames[pButton.name] ~= nil) then SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames[pButton.name]) end
					else
						SendChatMessage(".playerbot bot add " .. pButton.name, "SAY")
						pButton.setEnable()
					end
				end
			end
			
			-- MEMBERBOTS --
			
			for i = 1, 50 do
				local tName, tRank, tIndex, tLevel, tClass = GetGuildRosterInfo(i)
				
				-- Ensure that the Counter is not bigger than the Amount of Members in Guildlist
				if(tName ~= nil and tLevel ~= nil and tClass ~= nil and tName ~= UnitName("player")) then
					local tMember = SynthiqBotsUI.addMember(tClass, tLevel, tName).setDisable()
					
					tMember.doRight = function(pButton)
						if(pButton.state == false) then return end
						SendChatMessage(".playerbot bot remove " .. pButton.name, "SAY")
						if(pButton.parent.frames[pButton.name] ~= nil) then pButton.parent.frames[pButton.name]:Hide() end
						pButton.setDisable()
					end
					
					tMember.doLeft = function(pButton)
						if(pButton.state) then
							if(pButton.parent.frames[pButton.name] ~= nil) then SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames[pButton.name]) end
						else
							SendChatMessage(".playerbot bot add " .. pButton.name, "SAY")
							pButton.setEnable()
						end
					end
				else
					break
				end
			end
			
			-- FRIENDBOTS --
			
			for i = 1, 50 do
				local tName, tLevel, tClass = GetFriendInfo(i)
				
				-- Ensure that the Counter is not bigger than the Amount of Members in Friendlist
				if(tName ~= nil and tLevel ~= nil and tClass ~= nil and tName ~= UnitName("player")) then
					local tFriend = SynthiqBotsUI.addFriend(tClass, tLevel, tName).setDisable()
					
					tFriend.doRight = function(pButton)
						if(pButton.state == false) then return end
						SendChatMessage(".playerbot bot remove " .. pButton.name, "SAY")
						if(pButton.parent.frames[pButton.name] ~= nil) then pButton.parent.frames[pButton.name]:Hide() end
						pButton.setDisable()
					end
					
					tFriend.doLeft = function(pButton)
						if(pButton.state) then
							if(pButton.parent.frames[pButton.name] ~= nil) then SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames[pButton.name]) end
						else
							SendChatMessage(".playerbot bot add " .. pButton.name, "SAY")
							pButton.setEnable()
						end
					end
				else
					break
				end
			end
			
			-- REFRESH:RAID --
			
			if(GetNumRaidMembers() > 4) then
				for i = 1, GetNumRaidMembers() do
					local tName = UnitName("raid" .. i)
					SendChatMessage(".playerbot bot add " .. tName, "SAY")
				end
				
				return
			end
			
			-- REFRESH:GROUP --
			
			if(GetNumPartyMembers() > 0) then
				for i = 1, GetNumPartyMembers() do
					local tName = UnitName("party" .. i)
					SendChatMessage(".playerbot bot add " .. tName, "SAY")
				end
				
				return
			end
			
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "player already logged in")) then
			local tName = string.sub(arg1, 6, string.find(arg1, " ", 6) - 1)
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[tName]
			if(tButton == nil) then return end
			
			if(SynthiqBotsUI.isMember(tName)) then
				tButton.waitFor = "CO"
				SendChatMessage(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.combat, "NAME", tName), "SAY")
				SendChatMessage("co ?", "WHISPER", nil, tName)
				tButton.setEnable()
				--SynthiqBotsUI.doRaid()
				return
			end
			
			if(GetNumPartyMembers() == 4) then ConvertToRaid() end
			SynthiqBotsUI.doSlash("/invite", tName)
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "remove: ")) then
			local tName = string.sub(arg1, 9, string.find(arg1, " ", 9) - 1)
			local tFrame = SynthiqBotsUI.frames["MultiBar"].frames["Units"].frames[tName]
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[tName]
			if(tButton == nil) then return end
			
			if(SynthiqBotsUI.isInside(arg1, "not your bot")) then
				SendChatMessage("leave", "WHISPER", nil, tName)
			end
			
			SynthiqBotsUI.doRemove(SynthiqBotsUI.index.classes.actives[tButton.class], tButton.name)
			SynthiqBotsUI.doRemove(SynthiqBotsUI.index.actives, tButton.name)
			
			if(tFrame ~= nil) then tFrame:Hide() end
			tButton.setDisable()
			--SynthiqBotsUI.doRaid()
			return
		end
		
		if(arg1 == "Enable player botAI") then
			local tName = UnitName("player")
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[tName]
			if(tButton == nil) then return end
			tButton.waitFor = "CO"
			SendChatMessage(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.combat, "NAME", tName), "SAY")
			SendChatMessage("co ?", "WHISPER", nil, tName)
			tButton.setEnable()
			--SynthiqBotsUI.doRaid()
			return
		end
		
		if(arg1 == "Disable player botAI") then
			local tName = UnitName("player")
			local tFrame = SynthiqBotsUI.frames["MultiBar"].frames["Units"].frames[tName]
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[tName]
			if(tButton == nil) then return end
			if(tFrame ~= nil) then tFrame:Hide() end
			tButton.setDisable()
			--SynthiqBotsUI.doRaid()
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "Zone:", "zone:")) then
			local tPlayer = SynthiqBotsUI.getBot(UnitName("player"))
			if(tPlayer.waitFor ~= "COORDS") then return end
			
			local tLocation = SynthiqBotsUI.doSplit(arg1, " ")
			local tZone = string.sub(tLocation[6], 2, string.len(tLocation[6]) - 1)
			local tMap = string.sub(tLocation[3], 2, string.len(tLocation[3]) - 1)
			local tTip = SynthiqBotsUI.doReplace(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.teleport, "MAP", tMap), "ZONE", tZone)
			
			tPlayer.memory.goMap = tLocation[2]
			tPlayer.memory.tip = SynthiqBotsUI.doReplace(SynthiqBotsUI.tips.game.memory, "ABOUT", tTip)
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "X:") and SynthiqBotsUI.isInside(arg1, "Y:")) then
			local tPlayer = SynthiqBotsUI.getBot(UnitName("player"))
			if(tPlayer.waitFor ~= "COORDS") then return end
			
			local tCoords = SynthiqBotsUI.doSplit(arg1, " ")
			tPlayer.memory.goX = tCoords[2]
			tPlayer.memory.goY = tCoords[4]
			tPlayer.memory.goZ = tCoords[6]
			tPlayer.memory.setEnable()
			tPlayer.waitFor = ""
			return
		end
	end
	
	-- CHAT:WHISPER --
	
	if(event == "CHAT_MSG_WHISPER") then
		if(SynthiqBotsUI.auto.release == true) then
			-- Graveyard not ready to talk Bot in the chinese Version --
			if(arg1 == "在墓地见我") then
				SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[arg2].waitFor = "你好"
				return
			end
			
			if(arg1 == "Meet me at the graveyard") then
				SynthiqBotsUI.sendBotChat("summon", "WHISPER", nil, arg2)
				return
			end
		end
		
		if(SynthiqBotsUI.isInside(arg1, "StatsOfPlayer")) then
			local tUnit = SynthiqBotsUI.toUnit(arg2)
			SynthiqBotsUI.stats.frames[tUnit].setStats(arg2, UnitLevel(tUnit), arg1, true)
		end
		
		if(arg1 == "stats" and arg2 ~= UnitName("player")) then
			local tXP = math.floor(100.0 / UnitXPMax("player") * UnitXP("player"))
			local tMana = math.floor(100.0 / UnitManaMax("player") * UnitMana("player"))
			SynthiqBotsUI.sendBotChat("StatsOfPlayer " .. tXP .. " " .. tMana, "WHISPER", nil, arg2)
		end
		
		-- REQUIREMENT --
		
		local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[arg2]
		
		if(SynthiqBotsUI.auto.release == true) then
			-- Graveyard ready to talk Bot in the chinese Version --
			if(tButton ~= nil and tButton.waitFor == "你好" and arg1 == "你好") then
				SynthiqBotsUI.sendBotChat("summon", "WHISPER", nil, arg2)
				tButton.waitFor = ""
				return
			end
		end
		
		if(SynthiqBotsUI.isInside(arg1, "Hello", "你好") and tButton == nil) then
			local tUnit = SynthiqBotsUI.toUnit(arg2)
			local tLocClass, tClass = UnitClass(tUnit)
			local tLevel = UnitLevel(tUnit)
			
			tButton = SynthiqBotsUI.addActive(tClass, tLevel, arg2).setDisable()
			
			tButton.doRight = function(pButton)
				SendChatMessage(".playerbot bot remove " .. pButton.name, "SAY")
				if(pButton.parent.frames[pButton.name] ~= nil) then pButton.parent.frames[pButton.name]:Hide() end
				pButton.setDisable()
			end
					
			tButton.doLeft = function(pButton)
				if(pButton.state) then
					if(pButton.parent.frames[pButton.name] ~= nil) then SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames[pButton.name]) end
				else
					SendChatMessage(".playerbot bot add " .. pButton.name, "SAY")
					pButton.setEnable()
				end
			end
		elseif(tButton == nil) then return end
		
		if(SynthiqBotsUI.isInside(arg1, "Hello", "你好") and tButton.class == "Unknown" and tButton.roster == "friends") then
			local tName = ""
			local tLevel = ""
			local tClass = ""
			
			for i = 1, 50 do
				tName, tLevel, tClass = GetFriendInfo(i)
				if(tName == arg2) then break end
				if(tName == nil) then break end
			end
			
			local tClass = SynthiqBotsUI.toClass(tClass)
			local tTable = SynthiqBotsUI.index.classes[tButton.roster][tButton.class]
			local tIndex = 0
			
			for i = 1, table.getn(tTable) do
				if(tTable[i] == arg2) then 
					tIndex = i
					break
				end
			end
			
			if(tIndex > 0) then
				if(SynthiqBotsUI.index.classes[tButton.roster][tClass] == nil) then SynthiqBotsUI.index.classes[tButton.roster][tClass] = {} end
				table.remove(SynthiqBotsUI.index.classes[tButton.roster][tButton.class], tIndex)
				table.insert(SynthiqBotsUI.index.classes[tButton.roster][tClass], tName)
			end
			
			tButton.setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\class_" .. string.lower(tClass) .. ".blp")
			tButton.tip = SynthiqBotsUI.toTip(tClass, tLevel, tName)
			tButton.class = tClass
		end
		
		if(SynthiqBotsUI.isInside(arg1, "Hello", "你好")) then
			tButton.waitFor = "CO"
			SynthiqBotsUI.sendBotChat(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.combat, "NAME", arg2), "SAY")
			SynthiqBotsUI.sendBotChat("co ?", "WHISPER", nil, arg2)
			--SynthiqBotsUI.doRaid()
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "Goodbye", "再见")) then
			--SynthiqBotsUI.doRaid()
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "reset to default") and tButton.waitFor == "CO") then
			SynthiqBotsUI.sendBotChat("co ,?", "WHISPER", nil, arg2)
			return
		end
		
		if(SynthiqBotsUI.isInside(arg1, "reset to default") and tButton.waitFor == "NC") then
			SynthiqBotsUI.sendBotChat("nc ,?", "WHISPER", nil, arg2)
			return
		end
		
		if(tButton.waitFor == "DETAIL" and SynthiqBotsUI.isInside(arg1, "playing with")) then
			tButton.waitFor = ""
			SynthiqBotsUI.RaidPool(arg2, arg1)
			return
		end
		
		if(tButton.waitFor == "IGNORE" and SynthiqBotsUI.isInside(arg1, "Ignored ")) then
			if(SynthiqBotsUI.spells[arg2] == nil) then SynthiqBotsUI.spells[arg2] = {} end
			tButton.waitFor = "DETAIL"
			
			local tSpells = {}
			local tIgnores = SynthiqBotsUI.doSplit(arg1, ": ")[2]
			
			if(tIgnores ~= nil) then
				tSpells = SynthiqBotsUI.doSplit(tIgnores, ", ")
				
				for k,v in pairs(tSpells) do
					local tSpell = SynthiqBotsUI.doSplit(v, "|")[3]
					if(tSpell ~= nil) then SynthiqBotsUI.spells[arg2][SynthiqBotsUI.doSplit(tSpell, ":")[2]] = false end
				end
			end
			
			SynthiqBotsUI.sendBotChat("who", "WHISPER", nil, arg2)
			return
		end
		
		if(tButton.waitFor == "NC" and SynthiqBotsUI.isInside(arg1, "Strategies: ")) then
			tButton.waitFor = "IGNORE"
			tButton.normal = string.sub(arg1, 13)
			
			tFrame = SynthiqBotsUI.frames["MultiBar"].frames["Units"].addFrame(arg2, tButton.x - tButton.size - 2, tButton.y + 2)
			tFrame.class = tButton.class
			tFrame.name = tButton.name
			
			SynthiqBotsUI["add" .. tButton.class](tFrame, tButton.combat, tButton.normal)
			SynthiqBotsUI.addEvery(tFrame, tButton.combat, tButton.normal)
			
			if(SynthiqBotsUI.index.classes.actives[tButton.class] == nil) then SynthiqBotsUI.index.classes.actives[tButton.class] = {} end
			if(SynthiqBotsUI.isActive(tButton.name) == false) then
				table.insert(SynthiqBotsUI.index.classes.actives[tButton.class], tButton.name)
				table.insert(SynthiqBotsUI.index.actives, tButton.name)
			end
			
			tButton.setEnable()
			SynthiqBotsUI.sendBotChat("ss ?", "WHISPER", nil, arg2)
			return
		end
		
		if(tButton.waitFor == "CO" and SynthiqBotsUI.isInside(arg1, "Strategies: ")) then
			tButton.waitFor = "NC"
			tButton.combat = string.sub(arg1, 13)
			SynthiqBotsUI.sendBotChat(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.normal, "NAME", arg2), "SAY")
			SynthiqBotsUI.sendBotChat("nc ?", "WHISPER", nil, arg2)
			return
		end
		
		if(tButton.waitFor ~= "ITEM" and tButton.waitFor ~= "SPELL" and SynthiqBotsUI.auto.stats and SynthiqBotsUI.isInside(arg1, "Bag")) then
			local tUnit = SynthiqBotsUI.toUnit(arg2)
			if(SynthiqBotsUI.stats.frames[tUnit] == nil) then SynthiqBotsUI.addStats(SynthiqBotsUI.stats, "party1", 0, 0, 32, 192, 96) end
			SynthiqBotsUI.stats.frames[tUnit].setStats(arg2, UnitLevel(tUnit), arg1)
			return
		end
		
		-- Inventory --
		
		if(tButton.waitFor == "INVENTORY" and SynthiqBotsUI.isInside(arg1, "Inventory", "背包")) then
			local tItems = SynthiqBotsUI.inventory.frames["Items"]
			for key, value in pairs(tItems.buttons) do value:Hide() end
			table.wipe(tItems.buttons)
			SynthiqBotsUI.inventory.setText("Title", SynthiqBotsUI.doReplace(SynthiqBotsUI.info.inventory, "NAME", arg2))
			SynthiqBotsUI.inventory.name = arg2
			tItems.index = 0
			tButton.waitFor = "ITEM"
			SynthiqBotsUI.sendBotChat("stats", "WHISPER", nil, arg2)
			return
		end
		
		if(tButton.waitFor == "ITEM" and (SynthiqBotsUI.beInside(arg1, "Bag,", "Dur") or SynthiqBotsUI.beInside(arg1, "背包", "耐久度"))) then
			SynthiqBotsUI.inventory:Show()
			tButton.waitFor = ""
			InspectUnit(arg2)
			return
		end
		
		if(tButton.waitFor == "ITEM") then
			if(string.sub(arg1, 1, 3) == "---") then return end
			SynthiqBotsUI.addItem(SynthiqBotsUI.inventory.frames["Items"], arg1)
			return
		end
		
		-- Spellbook --
		
		if(tButton.waitFor == "SPELLBOOK" and SynthiqBotsUI.isInside(arg1, "Spells")) then
			local tOverlay = SynthiqBotsUI.spellbook.frames["Overlay"]
			local tSpellbook = SynthiqBotsUI.spellbook
			table.wipe(tSpellbook.spells)
			tSpellbook.frames["Overlay"].setText("Title", SynthiqBotsUI.doReplace(SynthiqBotsUI.info.spellbook, "NAME", arg2))
			tSpellbook.name = arg2
			tSpellbook.index = 0
			tSpellbook.from = 1
			tSpellbook.to = 16
			tButton.waitFor = "SPELL"
			SynthiqBotsUI.sendBotChat("stats", "WHISPER", nil, arg2)
			return
		end
		
		if(tButton.waitFor == "SPELL" and SynthiqBotsUI.isInside(arg1, "Bag,", "Dur", "XP", "背包", "耐久度", "经验值")) then
			local tOverlay = SynthiqBotsUI.spellbook.frames["Overlay"]
			local tSpellbook = SynthiqBotsUI.spellbook
			tSpellbook.now = 1
			tSpellbook.max = math.ceil(tSpellbook.index / 16)
			tOverlay.setText("Pages", "|cffffffff" .. tSpellbook.now .. "/" .. tSpellbook.max .. "|r")
			if(tSpellbook.now == tSpellbook.max) then tOverlay.buttons[">"].doHide() else tOverlay.buttons[">"].doShow() end
			tOverlay.buttons["<"].doHide()
			tSpellbook:Show()
			tButton.waitFor = ""
			InspectUnit(arg2)
			return
		end
		
		if(tButton.waitFor == "SPELL") then
			SynthiqBotsUI.addSpell(arg1, arg2)
			return
		end
		
		-- EQUIPPING --
		
		if(SynthiqBotsUI.inventory:IsVisible()) then
			if(SynthiqBotsUI.isInside(arg1, "装备", "使用", "吃", "喝", "盛宴", "摧毁")) then
				tButton.waitFor = "INVENTORY"
				SynthiqBotsUI.sendBotChat("items", "WHISPER", nil, tButton.name)
				return
			end
			
			if(SynthiqBotsUI.isInside(string.lower(arg1), "equipping", "using", "eating", "drinking", "feasting", "destroyed")) then
				tButton.waitFor = "INVENTORY"
				SynthiqBotsUI.sendBotChat("items", "WHISPER", nil, tButton.name)
				return
			end
			
			if(SynthiqBotsUI.inventory:IsVisible() and SynthiqBotsUI.isInside(string.lower(arg1), "opened")) then
				tButton.waitFor = "LOOT"
				return
			end
		end
		
		return
	end
	
	if(event == "CHAT_MSG_LOOT") then
		if(SynthiqBotsUI.inventory:IsVisible()) then
			local tButton = nil
			
			if(SynthiqBotsUI.isInside(arg1, "获得了物品")) then
				local tName = SynthiqBotsUI.doReplace(SynthiqBotsUI.doSplit(arg1, ":")[1], "获得了物品", "")
				tButton = SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[tName]
			end
			
			if(SynthiqBotsUI.isInside(string.lower(arg1), "beute", "receives")) then
				local tName = SynthiqBotsUI.doSplit(arg1, " ")[1]
				tButton = SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[tName]
			end
			
			if(tButton ~= nil and tButton.waitFor == "LOOT" and tButton ~= nil) then
				tButton.waitFor = "INVENTORY"
				SynthiqBotsUI.sendBotChat("items", "WHISPER", nil, tButton.name)
				return
			end
		end
		
		return
	end
	
	if(event == "TRADE_CLOSED") then
		if(SynthiqBotsUI.inventory:IsVisible()) then
			SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons[SynthiqBotsUI.inventory.name].waitFor = "INVENTORY"
			SynthiqBotsUI.sendBotChat("items", "WHISPER", nil, SynthiqBotsUI.inventory.name)
			return
		end
		
		return
	end
		
	-- QUEST:COMPLETE --
	
	if(event == "QUEST_COMPLETE") then
		if(SynthiqBotsUI.reward.state) then
			SynthiqBotsUI.setRewards()
			return
		end
		
		return
	end
	
	-- QUEST:CHANGED --
	
	if(event == "QUEST_LOG_UPDATE") then
		local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Right"].buttons["Quests"]
		tButton.doRight(tButton)
		return
	end
	
	-- WORLD:MAP --
	
	if(event == "WORLD_MAP_UPDATE") then
		if(SynthiqBotsUI.necronet.state == false) then return end
		
		local tCont = GetCurrentMapContinent()
		local tArea = GetCurrentMapAreaID()
		
		if(SynthiqBotsUI.necronet.cont ~= tCont or SynthiqBotsUI.necronet.area ~= tArea) then
			for key, value in pairs(SynthiqBotsUI.necronet.buttons) do value:Hide() end
			
			SynthiqBotsUI.necronet.cont = tCont
			SynthiqBotsUI.necronet.area = tArea
			
			local tTable = SynthiqBotsUI.necronet.index[tCont]
			if(tTable ~= nil) then tTable = tTable[tArea] end
			if(tTable ~= nil) then for key, value in pairs(tTable) do value:Show() end end
		end
		
		return
	end
end)

SLASH_SYNTHIQBOTS1 = "/synthiqbots"
SLASH_SYNTHIQBOTS2 = "/sbots"
SLASH_SYNTHIQBOTS3 = "/sb"

SlashCmdList["SYNTHIQBOTS"] = function(msg)
	local tArg = ""
	local tRest = ""
	if(msg ~= nil) then
		tArg, tRest = string.match(msg, "^(%S*)%s*(.-)%s*$")
		if(tArg == nil) then tArg = "" end
		if(tRest == nil) then tRest = "" end
		tArg = string.lower(tArg)
		tRest = string.lower(tRest)
	end

	if(tArg == "chat") then
		local tApply = function(pState)
			SynthiqBotsUI.auto.botCommands = pState
			SynthiqBotsUISave["AutoChatCommands"] = SynthiqBotsUI.IF(pState, "true", "false")
			local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["AutoChat"]
			if(tButton ~= nil) then
				if(pState) then tButton.setEnable() else tButton.setDisable() end
			end
		end
		if(tRest == "on" or tRest == "true" or tRest == "1") then
			tApply(true)
			print("|cff00ff00[SynthiqBotsUI]|r Auto chat: ON")
		elseif(tRest == "off" or tRest == "false" or tRest == "0") then
			tApply(false)
			print("|cff00ff00[SynthiqBotsUI]|r Auto chat: OFF")
		else
			local tStr = SynthiqBotsUI.IF(SynthiqBotsUI.auto.botCommands, "ON", "OFF")
			print("|cff00ff00[SynthiqBotsUI]|r Auto chat: " .. tStr .. " (use: /sb chat on|off|status)")
		end
		return
	end

	-- /sb feedback — the feedback module re-parses `msg` to preserve
	-- the original casing of the user's note (the parser above lowercases tRest).
	if(tArg == "feedback") then
		if(SynthiqBotsUI.feedback ~= nil and SynthiqBotsUI.feedback.handleSlash ~= nil) then
			SynthiqBotsUI.feedback.handleSlash(msg)
		end
		return
	end

	if(SynthiqBotsUI.state) then
		for key, value in pairs(SynthiqBotsUI.frames) do value:Hide() end
		SynthiqBotsUI.state = false
	else
		for key, value in pairs(SynthiqBotsUI.frames) do value:Show() end
		SynthiqBotsUI.state = true
	end
end