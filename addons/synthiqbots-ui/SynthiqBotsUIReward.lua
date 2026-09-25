SynthiqBotsUI.setRewards = function()
	if(SynthiqBotsUI.reward.state == false) then return end
	local tChoices = SynthiqBotsUI.IF(GetNumQuestChoices() > 6, 6, GetNumQuestChoices())
	table.wipe(SynthiqBotsUI.reward.rewards)
	table.wipe(SynthiqBotsUI.reward.units)
	SynthiqBotsUI.reward.rewards = {}
	SynthiqBotsUI.reward.units = {}
	
	for i = 1, tChoices do
		local tLink = GetQuestItemLink("CHOICE", i)
		local tName, tIcon = GetQuestItemInfo("CHOICE", i)
		SynthiqBotsUI.reward.rewards[i] = { tLink, tName, tIcon }
	end
	
	for i = 1, 12 do
		local tID = "U" .. SynthiqBotsUI.IF(i < 10, "0", "") .. i
		local tUnit = SynthiqBotsUI.reward.frames["Overlay"].frames[tID]
		for j = 1, 6 do tUnit.buttons["R" .. j]:Hide() end
		tUnit:Hide()
	end
	
	if(GetNumRaidMembers() > 0) then
		for i = 1, 40 do
			local tUnit = UnitName("raid" .. i)
			if(tUnit ~= nil) then
				local tBot = SynthiqBotsUI.getBot(tUnit)
				if(tBot ~= nil and tBot.name ~= UnitName("player")) then table.insert(SynthiqBotsUI.reward.units, tBot) end
			end
		end
	elseif(GetNumPartyMembers() > 0) then
		for i = 1, 5 do
			local tUnit = UnitName("party" .. i)
			if(tUnit ~= nil) then
				local tBot = SynthiqBotsUI.getBot(tUnit)
				if(tBot ~= nil and tBot.name ~= UnitName("player")) then table.insert(SynthiqBotsUI.reward.units, tBot) end
			end
		end
	end
	
	if(table.getn(SynthiqBotsUI.reward.units) > 0 and tChoices > 0) then
		local tOverlay = SynthiqBotsUI.reward.frames["Overlay"]
		local tUnits = table.getn(SynthiqBotsUI.reward.units)
		
		SynthiqBotsUI.reward.max = math.ceil(tUnits / SynthiqBotsUI.reward.to)
		tOverlay.setText("Pages", SynthiqBotsUI.reward.now .. "/" .. SynthiqBotsUI.reward.max)
		tOverlay.buttons["<"]:Show()
		tOverlay.buttons[">"]:Show()
		
		if(SynthiqBotsUI.reward.now == 1) then tOverlay.buttons["<"]:Hide() end
		if(SynthiqBotsUI.reward.now == SynthiqBotsUI.reward.max) then tOverlay.buttons[">"]:Hide() end
		
		if(tUnits > SynthiqBotsUI.reward.to) then tUnits = SynthiqBotsUI.reward.to end
		
		for i = 1, tUnits do
			local tBot = SynthiqBotsUI.reward.units[i]
			local tUnit = SynthiqBotsUI.setReward(i, tBot, false)
			
			for j = 1, tChoices do
				local tReward = tUnit.buttons["R" .. j]
				tReward:Show()
				tReward.link = SynthiqBotsUI.reward.rewards[j][1]
				tReward.setButton(SynthiqBotsUI.reward.rewards[j][3], SynthiqBotsUI.reward.rewards[j][1])
				tReward.doLeft = function(pButton)
					pButton.parent:Hide()
					SendChatMessage("r " .. pButton.link, "WHISPER", nil, pButton.getName())
					SynthiqBotsUI.getBot(pButton.getName()).rewarded = true
					SynthiqBotsUI.reward.doClose()
				end
			end
		end
		
		SynthiqBotsUI.reward:Show()
	end
end

SynthiqBotsUI.setReward = function(pIndex, pBot, oRewarded)
	local tID = "U" .. SynthiqBotsUI.IF(pIndex < 10, "0", "") .. pIndex
	local tUnit = SynthiqBotsUI.reward.frames["Overlay"].frames[tID]
	if(oRewarded ~= nil) then pBot.rewarded = oRewarded end
	if(pBot.rewarded) then tUnit:Hide() else tUnit:Show() end
	tUnit.setText(tID, "|cffffcc00" .. pBot.name .. " - " .. pBot.class .. "|r")
	tUnit.class = pBot.class
	tUnit.name = pBot.name
	return tUnit
end