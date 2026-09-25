SynthiqBotsUI.addStats = function(pFrame, pIndex, pX, pY, pSize, pWidth, pHeight)
	local tFrame = pFrame.addFrame(pIndex, pX, pY, pSize, pWidth, pHeight)
	local tAddon = tFrame.addFrame("Addon", -2, 46, 48)
	tAddon.addTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_99_percent.blp")
	tFrame.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Stats.blp")
	tFrame:Hide()
	
	tFrame.addText("Name", "", "TOPLEFT", 54, -11, 11)
	tFrame.addText("Values", "", "TOPLEFT", 54, -27, 11)
	tAddon.addText("Percent", "", "CENTER", 0,  0, 11)
	tFrame.addText("Level", "", "CENTER", 85.25, 5, 11)
	
	tFrame.setProgress = function(pFrame, pProgress)
		pFrame.frames["Addon"].texture:Hide()
		
		if(pProgress >= 99) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_99_percent.blp")
		elseif(pProgress >= 90) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_90_percent.blp")
		elseif(pProgress >= 81) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_81_percent.blp")
		elseif(pProgress >= 72) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_72_percent.blp")
		elseif(pProgress >= 63) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_63_percent.blp")
		elseif(pProgress >= 54) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_54_percent.blp")
		elseif(pProgress >= 45) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_45_percent.blp")
		elseif(pProgress >= 36) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_36_percent.blp")
		elseif(pProgress >= 27) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_27_percent.blp")
		elseif(pProgress >= 18) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_18_percent.blp")
		elseif(pProgress >=  9) then pFrame.frames["Addon"].setTexture("Interface\\AddOns\\synthiqbots-ui\\Icons\\xp_progress_9_percent.blp")
		end
		
		return pProgress
	end
	
	tFrame.setStats = function(pName, pLevel, pStats, oPlayer)
		local tFrame = SynthiqBotsUI.stats.frames[SynthiqBotsUI.toUnit(pName)]
		local tAddon = tFrame.frames["Addon"]
		local tChina = GetLocale() == "zhCN"
		
		if(oPlayer ~= nil and oPlayer == true) then
			local tStats = SynthiqBotsUI.doSplit(pStats, ", ")
			local tMana = tonumber(tStats[5])
			local tXP = tonumber(tStats[4])
			
			tFrame.texts["Name"]:SetText(pName)
			tFrame.texts["Level"]:SetText(pLevel)
			tFrame.texts["Values"]:SetText("Player")
			
			if(pLevel == 80) then
				tAddon.texts["Percent"]:SetText(tFrame.setProgress(tFrame, tMana) .. "%\n" .. SynthiqBotsUI.info.shorts.mp)
			else
				tAddon.texts["Percent"]:SetText(tFrame.setProgress(tFrame, tXP) .. "%\n" .. SynthiqBotsUI.info.shorts.xp)
			end
			
			tFrame:Show()
			return
		end
		
		local tStats = SynthiqBotsUI.doSplit(pStats, ", ")
		local tMoney = "|cffffdd55" .. tStats[1] .. "|r, "
		local tBag = SynthiqBotsUI.IF(tChina, SynthiqBotsUI.doReplace(tStats[2], "Bag", SynthiqBotsUI.info.shorts.bag), tStats[2])
		
		tFrame.texts["Name"]:SetText(pName)
		tFrame.texts["Level"]:SetText(pLevel)
		tFrame.texts["Values"]:SetText(tMoney .. tBag)
		
		if(pLevel == 80) then
			local tDur = SynthiqBotsUI.doSplit(string.sub(SynthiqBotsUI.doSplit(tStats[3], "|")[2], 10), " ")
			local tQuality = tonumber(string.sub(tDur[1], 1, string.len(tDur[1]) - 1))
			local tRepair = tonumber(string.sub(tDur[2], 2, string.len(tDur[2]) - 1))
			if(tQuality == 0 and tRepair == 0) then tQuality = 100 end
			tAddon.texts["Percent"]:SetText(tFrame.setProgress(tFrame, tQuality) .. "%\n" .. SynthiqBotsUI.info.shorts.dur)
		else
			local tXP = tonumber(string.sub(SynthiqBotsUI.doSplit(tStats[4], "|")[2], 10))
			tAddon.texts["Percent"]:SetText(tFrame.setProgress(tFrame, tXP) .. "%\n" .. SynthiqBotsUI.info.shorts.xp)
		end
		
		tFrame:Show()
		return
	end
end