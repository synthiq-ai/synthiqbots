SynthiqBotsUI.raidus = SynthiqBotsUI.newFrame(SynthiqBotsUI, -340, -126, 32, 884, 884)
SynthiqBotsUI.raidus.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Raidus.blp")
SynthiqBotsUI.raidus:SetMovable(true)
SynthiqBotsUI.raidus:Hide()

SynthiqBotsUI.raidus.addFrame("Pool", -20, 360, 28, 160, 490)
SynthiqBotsUI.raidus.addFrame("Btop", -35, 822, 24, 128, 32).addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Raidus_Banner_Top.blp")
SynthiqBotsUI.raidus.addFrame("Bbot", -35, 354, 24, 128, 32).addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Raidus_Banner_Bottom.blp")
SynthiqBotsUI.raidus.addFrame("Group8", -185, 364, 28, 160, 240)
SynthiqBotsUI.raidus.addFrame("Group7", -350, 364, 28, 160, 240)
SynthiqBotsUI.raidus.addFrame("Group6", -515, 364, 28, 160, 240)
SynthiqBotsUI.raidus.addFrame("Group5", -680, 364, 28, 160, 240)
SynthiqBotsUI.raidus.addFrame("Group4", -185, 604, 28, 160, 240)
SynthiqBotsUI.raidus.addFrame("Group3", -350, 604, 28, 160, 240)
SynthiqBotsUI.raidus.addFrame("Group2", -515, 604, 28, 160, 240)
SynthiqBotsUI.raidus.addFrame("Group1", -680, 604, 28, 160, 240)
SynthiqBotsUI.raidus.addText("RaidScore", "RaidScore: 0", "BOTTOMLEFT", 376, 364, 12)
SynthiqBotsUI.raidus.save = ""
SynthiqBotsUI.raidus.from = 1
SynthiqBotsUI.raidus.to = 11

SynthiqBotsUI.raidus.movButton("Move", -780, 790, 90, SynthiqBotsUI.tips.move.raidus)

SynthiqBotsUI.raidus.wowButton("x", -13, 841, 16, 20, 12)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].frames["Main"].buttons["Raidus"]
	tButton.doLeft(tButton)
end

SynthiqBotsUI.raidus.wowButton("Load", -762, 360, 80, 20, 12)
.doLeft = function(pButton)
	local tPool = SynthiqBotsUI.raidus.frames["Pool"]
	local tData = SynthiqBotsUISave["Raidus" .. SynthiqBotsUI.raidus.save]
	
	if(tData == nil or tData == "") then
		SendChatMessage(SynthiqBotsUI.info.nothing, "SAY");
	end
	
	local tLoad = SynthiqBotsUI.doSplit(tData, ";")
	
	for i = 1, 8, 1 do
		local tGroup = SynthiqBotsUI.doSplit(tLoad[i], ",")
		
		for j = 1, 5, 1 do
			local tDrop = SynthiqBotsUI.raidus.frames["Group" .. i].frames["Slot" .. j]
			local tName = tGroup[j]
			
			if(tName ~= "-") then
				for tIndex, tDrag in pairs(tPool.frames) do
					if(tDrag.name ~= nil and tDrag.name == tName) then
						local tVisible = tDrag:IsVisible()
						local tParent = tDrag.parent
						local tHeight = tDrag.height
						local tWidth = tDrag.width
						local tSlot = tDrag.slot
						local tX = tDrag.x
						local tY = tDrag.y
						
						SynthiqBotsUI.raidus.doDrop(tDrag, tDrop.parent, tDrop.x, tDrop.y, tDrop.width, tDrop.height, tDrop.slot)
						if(tDrop:IsVisible()) then tDrag:Show() else tDrag:Hide() end
						
						SynthiqBotsUI.raidus.doDrop(tDrop, tParent, tX, tY, tWidth, tHeight, tSlot)
						if(tVisible) then tDrop:Show() else tDrop:Hide() end
					end
				end
			end
		end
	end
end

SynthiqBotsUI.raidus.wowButton("1", -734, 360, 22, 20, 12).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		pButton.parent.save = ""
		pButton.setDisable()
		SynthiqBotsUI.raidus.setRaidus()
	else
		pButton.parent.save = "1"
		pButton.parent.buttons["2"].setDisable()
		pButton.parent.buttons["3"].setDisable()
		pButton.setEnable()
		SynthiqBotsUI.raidus.setRaidus()
	end
end

SynthiqBotsUI.raidus.wowButton("2", -707, 360, 22, 20, 12).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		pButton.parent.save = ""
		pButton.setDisable()
		SynthiqBotsUI.raidus.setRaidus()
	else
		pButton.parent.save = "2"
		pButton.parent.buttons["1"].setDisable()
		pButton.parent.buttons["3"].setDisable()
		pButton.setEnable()
		SynthiqBotsUI.raidus.setRaidus()
	end
end

SynthiqBotsUI.raidus.wowButton("3", -680, 360, 22, 20, 12).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		pButton.parent.save = ""
		pButton.setDisable()
		SynthiqBotsUI.raidus.setRaidus()
	else
		pButton.parent.save = "3"
		pButton.parent.buttons["1"].setDisable()
		pButton.parent.buttons["2"].setDisable()
		pButton.setEnable()
		SynthiqBotsUI.raidus.setRaidus()
	end
end

SynthiqBotsUI.raidus.wowButton("Save", -597, 360, 80, 20, 12)
.doLeft = function(pButton)
	local tSave = ""
	
	for i = 1, 8, 1 do
		local tGroup = ""
		
		for j = 1, 5, 1 do
			local tSlot = SynthiqBotsUI.raidus.frames["Group" .. i].frames["Slot" .. j]
			local tName = SynthiqBotsUI.IF(tSlot.name == nil, "-", tSlot.name)
			tGroup = tGroup .. SynthiqBotsUI.IF(tGroup == "", "", ",")
			tGroup = tGroup .. tName
		end
		
		tSave = tSave .. SynthiqBotsUI.IF(tSave == "", "", ";")
		tSave = tSave .. tGroup
	end
	
	SynthiqBotsUISave["Raidus" .. SynthiqBotsUI.raidus.save] = tSave
	SendChatMessage("I wrote it down.", "SAY")
end

SynthiqBotsUI.raidus.wowButton("Apply", -514, 360, 80, 20, 12)
.doLeft = function(pButton)
	local tRaidByIndex, tRaidByName = SynthiqBotsUI.raidus.getRaidTarget()
	if(tRaidByIndex == nil or tRaidByName == nil) then return end
	
	local tSelf = UnitName("player")
	SynthiqBotsUI.index.raidus = {}
	
	for tName, tValue in pairs(SynthiqBotsUI.frames["MultiBar"].frames["Units"].buttons) do
		if(tValue.state) then
			if(tName ~= tSelf and tRaidByName[tName] == nil) then
				if(UnitInGroup(tName) or UnitInRaid(tName)) then UninviteUnit(tName) end
				SendChatMessage(".playerbot bot remove " .. tName, "SAY")
			end
		else
			if(tName ~= tSelf and tRaidByName[tName] ~= nil) then
				table.insert(SynthiqBotsUI.index.raidus, tName)
			end
		end
	end
	
	local tNeeds = table.getn(SynthiqBotsUI.index.raidus)
	
	if(tNeeds > 0) then
		SendChatMessage(SynthiqBotsUI.info.starting, "SAY")
		SynthiqBotsUI.timer.invite.roster = "raidus"
		SynthiqBotsUI.timer.invite.needs = tNeeds
		SynthiqBotsUI.timer.invite.index = 1
		SynthiqBotsUI.auto.invite = true
	else
		SynthiqBotsUI.timer.sort.elapsed = 0
		SynthiqBotsUI.timer.sort.index = 1
		SynthiqBotsUI.timer.sort.needs = 0
		SynthiqBotsUI.auto.sort = true
	end
end

SynthiqBotsUI.raidus.wowButton("<", -40, 360, 16, 20, 12)
.doLeft = function(pButton)
	for k,v in pairs(SynthiqBotsUI.raidus.frames["Pool"].frames) do v:Hide() end
	
	SynthiqBotsUI.raidus.from = SynthiqBotsUI.raidus.from - 11
	SynthiqBotsUI.raidus.to = SynthiqBotsUI.raidus.to - 11
	
	if(SynthiqBotsUI.raidus.to < 1) then
		SynthiqBotsUI.raidus.from = SynthiqBotsUI.raidus.slots - 10
		SynthiqBotsUI.raidus.to = SynthiqBotsUI.raidus.slots
	end
	
	for i = 1, SynthiqBotsUI.raidus.slots, 1 do
		local tSlot = SynthiqBotsUI.raidus.frames["Pool"].frames["Slot" .. i]
		if(i >= SynthiqBotsUI.raidus.from and i <= SynthiqBotsUI.raidus.to) then tSlot:Show() else tSlot:Hide() end
	end
end

SynthiqBotsUI.raidus.wowButton(">", -20, 360, 16, 20, 12)
.doLeft = function(pButton)
	SynthiqBotsUI.raidus.from = SynthiqBotsUI.raidus.from + 11
	SynthiqBotsUI.raidus.to = SynthiqBotsUI.raidus.to + 11
	
	if(SynthiqBotsUI.raidus.from > SynthiqBotsUI.raidus.slots) then
		SynthiqBotsUI.raidus.from = 1
		SynthiqBotsUI.raidus.to = 11
	end
	
	for i = 1, SynthiqBotsUI.raidus.slots, 1 do
		local tSlot = SynthiqBotsUI.raidus.frames["Pool"].frames["Slot" .. i]
		if(i >= SynthiqBotsUI.raidus.from and i <= SynthiqBotsUI.raidus.to) then tSlot:Show() else tSlot:Hide() end
	end
end

SynthiqBotsUI.raidus.getDrop = function()
	for i = 1, 8, 1 do
		local tGroup = SynthiqBotsUI.raidus.frames["Group" .. i]
		
		if(MouseIsOver(tGroup)) then
			for j = 1, 5, 1 do
				local tSlot = tGroup.frames["Slot" .. j]
				if(MouseIsOver(tSlot)) then return tSlot end
			end
		end
	end
	
	for i = 1, SynthiqBotsUI.raidus.slots, 1 do
		local tSlot = SynthiqBotsUI.raidus.frames["Pool"].frames["Slot" .. i]
		if(MouseIsOver(tSlot)) then return tSlot end
	end
	
	return nil
end

-- SETTTER --

SynthiqBotsUI.raidus.setRaidus = function()
	local tPool = SynthiqBotsUI.raidus.frames["Pool"]
	local tSlot = 1
	local tY = 426
	
	for k,v in pairs(tPool.frames) do v:Hide() end
	
	local tBots = {}
	local tIndex = 1
	
	for tName, tValue in pairs(SynthiqBotsUIGlobalSave) do
		local tDetails = SynthiqBotsUI.doSplit(tValue, ",")
		local tBot = {}
		
		tBot.name = tName
		tBot.race = tDetails[1]
		tBot.gender = tDetails[2]
		tBot.special = tDetails[3]
		tBot.talents = tDetails[4]
		tBot.class = tDetails[5]
		tBot.level = tonumber(tDetails[6]) or 0
		tBot.score = tonumber(tDetails[7]) or 0
		
		local tClass = SynthiqBotsUI.toClass(tBot.class)
		
		tBot.sort = tBot.level * 1000
		+ SynthiqBotsUI.IF(tClass == "DeathKnight", 1100000
		, SynthiqBotsUI.IF(tClass == "Druid", 1200000
		, SynthiqBotsUI.IF(tClass == "Hunter", 1300000
		, SynthiqBotsUI.IF(tClass == "Mage", 1400000
		, SynthiqBotsUI.IF(tClass == "Paladin", 1500000
		, SynthiqBotsUI.IF(tClass == "Priest", 1600000
		, SynthiqBotsUI.IF(tClass == "Rogue", 1700000
		, SynthiqBotsUI.IF(tClass == "Shaman", 1800000
		, SynthiqBotsUI.IF(tClass == "Warlock", 1900000
		, SynthiqBotsUI.IF(tClass == "Warrior", 2000000
		, 1000000)))))))))) + tBot.score;
		
		tBots[tIndex] = tBot
		tIndex = tIndex + 1
	end
	
	for tIndex = 1, table.getn(tBots) do
		local tMax = tIndex
		
		for tSearch = tIndex + 1, table.getn(tBots) do
			if(tBots[tMax].sort < tBots[tSearch].sort) then
				tMax = tSearch
			end
		end
		
		tBots[tIndex], tBots[tMax] = tBots[tMax], tBots[tIndex]
	end
	
	for tIndex = 1, table.getn(tBots) do
		local tBot = tBots[tIndex]
		
		local tFrame = tPool.addFrame("Slot" .. tSlot, 0, tY, 28, 160, 36)
		tFrame.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\grey.blp")
		tFrame:SetResizable(false)
		tFrame:SetMovable(true)
		tFrame.class = SynthiqBotsUI.toClass(tBot.class)
		tFrame.slot = "Slot" .. tSlot
		tFrame.name = tBot.name
		tFrame.bot = tBot
		
		local tButton = tFrame.addButton("Icon", -128, 3, "Interface\\AddOns\\synthiqbots-ui\\Icons\\class_" .. strlower(tFrame.class) .. ".blp", "")
		tButton.doRight = function(pButton)
			SendChatMessage(".playerbot bot add " .. pButton.parent.name, "SAY")
		end
		
		tButton:SetScript("OnEnter", function(pButton)
			local tBot = pButton.parent.bot
			local tReward = tBot.level .. "." .. SynthiqBotsUI.IF(tBot.score < 100, "0", SynthiqBotsUI.IF(tBot.score < 10, "00", "")) .. tBot.score
			pButton.tip = SynthiqBotsUI.newFrame(pButton, -pButton.size, 160, 28, 256, 512, "TOPRIGHT")
			pButton.tip.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Raidus_Wanted.blp")
			pButton.tip.addModel(tBot.name, 0, 64, 160, 240, 1.0)
			pButton.tip.addText("1", "|cff555555- WANTED -|h", "TOP", 0, -30, 24)
			pButton.tip.addText("2", "|cff555555-DEAD OR ALIVE-|h", "TOP", 0, -55, 24)
			pButton.tip.addText("3", "|cff333333" .. tBot.name .. " - " .. tBot.gender .. " - " .. tBot.race .. "|h", "BOTTOM", 0, 220, 15)
			pButton.tip.addText("4", "|cff333333" .. tBot.class .. " - " .. tBot.talents .. " - " .. tBot.special .. "|h", "BOTTOM", 0, 200, 15)
			pButton.tip.addText("5", "|cff555555--------------------------------------------|h", "BOTTOM", 0, 188, 15)
			pButton.tip.addText("6", "|cff555555CASH - " .. tReward .. " - GOLD|h", "BOTTOM", 0, 170, 20)
			pButton.tip.addText("7", "|cff555555--------------------------------------------|h", "BOTTOM", 0, 160, 15)
			pButton.tip:Show()
		end)
		
		tButton:SetScript("OnMouseDown", function(pButton)
			pButton.parent:StartMoving()
			pButton.parent.isMoving = true
		end)
		
		tButton:SetScript("OnMouseUp", function(pButton)
			pButton.parent:StopMovingOrSizing()
			pButton.parent.isMoving = false
			
			local tDrag = pButton.parent
			local tDrop = SynthiqBotsUI.raidus.getDrop()
			
			if(tDrop ~= nil) then
				local tParent = tDrag.parent
				local tHeight = tDrag.height
				local tWidth = tDrag.width
				local tSlot = tDrag.slot
				local tX = tDrag.x
				local tY = tDrag.y
				
				SynthiqBotsUI.raidus.doDrop(tDrag, tDrop.parent, tDrop.x, tDrop.y, tDrop.width, tDrop.height, tDrop.slot)
				SynthiqBotsUI.raidus.doDrop(tDrop, tParent, tX, tY, tWidth, tHeight, tSlot)
			else
				pButton.parent:ClearAllPoints()
				pButton.parent:SetPoint(pButton.parent.align, pButton.parent.x, pButton.parent.y)
				pButton.parent:SetSize(pButton.parent.width, pButton.parent.height)
			end
		end)
		
		tFrame.addText("1", tBot.level .. " - " .. tBot.class, "BOTTOMLEFT", 36, 18, 12)
		tFrame.addText("2", tBot.score .. " - " .. tBot.special, "BOTTOMLEFT", 36, 6, 12)
		
		if(tSlot > 11) then tFrame:Hide() else tFrame:Show() end
		tY = SynthiqBotsUI.IF(mod(tSlot, 11) == 0, 426, tY - 40)
		tSlot = tSlot + 1
	end
	
	for i = mod(tSlot, 11), 11, 1 do
		local tFrame = tPool.addFrame("Slot" .. tSlot, 0, tY, 28, 160, 36)
		tFrame.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\grey.blp")
		tFrame.slot = "Slot" .. tSlot
		if(tSlot > 11) then tFrame:Hide() else tFrame:Show() end
		tSlot = tSlot + 1
		tY = tY - 40
	end
	
	SynthiqBotsUI.raidus.slots = tSlot - 1
	
	for i = 1, 8, 1 do
		local tGroup = SynthiqBotsUI.raidus.frames["Group" .. i]
		local tY = 182
		
		tGroup.addText("Title", "- Group" .. i .. " : 0 -", "BOTTOM", 0, 223, 12)
		tGroup.group = "Group" .. i
		tGroup.score = 0
		
		for j = 1, 5, 1 do
			local tFrame = tGroup.addFrame("Slot" .. j, 0, tY, 28, 160, 36)
			tFrame.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\grey.blp")
			tFrame.slot = "Slot" .. j
			tY = tY - 40
		end
	end
end

-- GETTER --

SynthiqBotsUI.raidus.getRaidState = function()
	local tRaidByMembers = {}
	local tRaidByGroups = {}
	
	local tRaid = GetNumRaidMembers()
	local tGroup = GetNumPartyMembers()
	local tAmount = SynthiqBotsUI.IF(tRaid > tGroup, tRaid, tGroup)
	
	for tIndex = 1, tAmount do
		local xName, xRank, xGroup = GetRaidRosterInfo(tIndex)
		if(xName and xRank and xGroup) then
			tRaidByMembers[xName] = { index = tIndex, group = xGroup }
			tRaidByGroups[xGroup] = (tRaidByGroups[xGroup] or 0) + 1
		end
	end
	
	return tRaidByMembers, tRaidByGroups
end

SynthiqBotsUI.raidus.getRaidTarget = function()
	local tRaidByIndex = {}
	local tRaidByName = {}
	local tIndex = 1
	
	local tSelf = UnitName("player")
	local tUser = true
	local tBots = true
	
	for tGroup = 1, 8 do
		for tSlot = 1, 5 do
			local tName = SynthiqBotsUI.raidus.frames["Group" .. tGroup].frames["Slot" .. tSlot].name
			if(tName ~= nil) then
				if(tName == tSelf) then tUser = false end
				tRaidByIndex[tIndex] = { name = tName, group = tGroup }
				tRaidByName[tName] = tGroup
				tIndex = tIndex + 1
				tBots = false
			end
		end
	end
	
	if(tBots) then return SendChatMessage("There is no Bot in the Raid", "SAY") end
	if(tUser) then return SendChatMessage("I must be in the Raid!", "SAY") end
	return tRaidByIndex, tRaidByName
end

-- EVENTS --

SynthiqBotsUI.raidus.doRaidSortCheck = function()
	local tRaidByIndex, tRaidByName = SynthiqBotsUI.raidus.getRaidTarget()
	local tRaidByMembers, tRaidByGroups = SynthiqBotsUI.raidus.getRaidState()
	
	for tName, tGroup in pairs(tRaidByName) do
		if(tRaidByMembers[tName] ~= nil and tRaidByMembers[tName].group ~= tGroup) then return 1 end
	end
	
	return nil
end

SynthiqBotsUI.raidus.doRaidSort = function(pIndex)
	local tRaidByIndex, tRaidByName = SynthiqBotsUI.raidus.getRaidTarget()
	local tRaidByMembers, tRaidByGroups = SynthiqBotsUI.raidus.getRaidState()
	
	if(pIndex > table.getn(tRaidByIndex)) then return nil end
	
	local tName = tRaidByIndex[pIndex].name
	local tGroup = tRaidByIndex[pIndex].group
	
	if(tRaidByMembers[tName] ~= nil and tRaidByMembers[tName].group ~= tGroup) then
		if(tRaidByGroups[tGroup] == nil) then
			SetRaidSubgroup(tRaidByMembers[tName].index, tGroup)
		else
			if(tRaidByGroups[tGroup] < 5) then
				SetRaidSubgroup(tRaidByMembers[tName].index, tGroup)
			else
				for xName, xValue in pairs(tRaidByMembers) do
					if(xValue.group == tGroup and tRaidByName[xName] ~= tGroup) then
						SwapRaidSubgroup(tRaidByMembers[tName].index, xValue.index)
						break
					end
				end
			end
		end
	end
	
	return pIndex + 1
end

SynthiqBotsUI.raidus.doGroupScore = function(pGroup)
	if(pGroup == nil or pGroup.group == nil) then return end
	local tScore = 0
	local tSize = 0
	
	for tKey, tSlot in pairs(pGroup.frames) do
		if(tSlot ~= nil and tSlot.bot ~= nil) then
			tScore = tScore + tSlot.bot.score
			tSize = tSize + 1
		end
	end
	
	pGroup.score = SynthiqBotsUI.IF(tSize > 0, math.floor(tScore / tSize), 0)
	pGroup.setText("Title", "- " .. pGroup.group .. " : " .. pGroup.score .. " -")
end

SynthiqBotsUI.raidus.doRaidScore = function()
	local tScore = 0
	local tSize = 0
	
	for tKey, tGroup in pairs(SynthiqBotsUI.raidus.frames) do
		if(tGroup ~= nil and tGroup.score ~= nil and tGroup.score > 0) then
			tScore = tScore + tGroup.score
			tSize = tSize + 1
		end
	end
	
	tScore = SynthiqBotsUI.IF(tSize > 0, math.floor(tScore / tSize), 0)
	SynthiqBotsUI.raidus.setText("RaidScore", "RaidScore: " .. tScore)
end

SynthiqBotsUI.raidus.doDrop = function(pObject, pParent, pX, pY, pWidth, pHeight, pSlot)
	pParent.frames[pSlot] = pObject
	pObject:ClearAllPoints()
	pObject:SetParent(pParent)
	pObject:SetPoint("BOTTOMRIGHT", pX, pY)
	pObject:SetSize(pWidth, pHeight)
	pObject.parent = pParent
	pObject.height = pHeight
	pObject.width = pWidth
	pObject.slot = pSlot
	pObject.x = pX
	pObject.y = pY
	SynthiqBotsUI.raidus.doGroupScore(pParent)
	SynthiqBotsUI.raidus.doRaidScore()
end