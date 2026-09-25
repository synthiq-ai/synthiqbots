SynthiqBotsUI.addEvery = function(pFrame, pCombat, pNormal)
	pFrame.addButton("Summon", 64, 0, "ability_hunter_beastcall", SynthiqBotsUI.tips.every.summon)
	.doLeft = function(pButton)
		SynthiqBotsUI.ActionToTarget("summon", pButton.getName())
	end
	
	pFrame.addButton("Uninvite", 94, 0, "inv_misc_grouplooking", SynthiqBotsUI.tips.every.uninvite).doShow()
	.doLeft = function(pButton)
		SynthiqBotsUI.doSlash("/uninvite", pButton.getName())
		pButton.getButton("Invite").doShow()
		pButton.doHide()
	end
	
	pFrame.addButton("Invite", 94, 0, "inv_misc_groupneedmore", SynthiqBotsUI.tips.every.invite).doHide()
	.doLeft = function(pButton)
		SynthiqBotsUI.doSlash("/invite", pButton.getName())
		pButton.getButton("Uninvite").doShow()
		pButton.doHide()
	end
	
	pFrame.addButton("Food", 124, 0, "inv_drink_24_sealwhey", SynthiqBotsUI.tips.every.food).setDisable()
	.doLeft = function(pButton)
		SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +food,?", "nc -food,?", pButton.getName())
	end
	
	pFrame.addButton("Loot", 154, 0, "inv_misc_coin_16", SynthiqBotsUI.tips.every.loot).setDisable()
	.doLeft = function(pButton)
		SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +loot,?", "nc -loot,?", pButton.getName())
	end
	
	pFrame.addButton("Gather", 184, 0, "trade_mining", SynthiqBotsUI.tips.every.gather).setDisable()
	.doLeft = function(pButton)
		SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +gather,?", "nc -gather,?", pButton.getName())
	end
	
	-- Selfbot is not allowed to use these Tools --
	if(pFrame.getName() == UnitName("player")) then return end
	
	pFrame.addButton("Inventory", 214, 0, "inv_misc_bag_08", SynthiqBotsUI.tips.every.inventory).setDisable()
	.doLeft = function(pButton)
		if(pButton.state) then
			SynthiqBotsUI.inventory:Hide()
			pButton.setDisable()
		else
			local tUnits = SynthiqBotsUI.frames["MultiBar"].frames["Units"]
			for key, value in pairs(SynthiqBotsUI.index.actives) do 
				if(tUnits.buttons[value].name ~= UnitName("player")) then
					tUnits.frames[value].getButton("Inventory").setDisable()
				end
			end
			
			pButton.setEnable()
			SynthiqBotsUI.inventory.name = pButton.getName()
			tUnits.buttons[SynthiqBotsUI.inventory.name].waitFor = "INVENTORY"
			SendChatMessage("items", "WHISPER", nil, pButton.getName())
		end
	end
	
	pFrame.addButton("Spellbook", 244, 0, "inv_misc_book_09", SynthiqBotsUI.tips.every.spellbook).setDisable()
	.doLeft = function(pButton)
		if(pButton.state) then
			SynthiqBotsUI.spellbook:Hide()
			pButton.setDisable()
		else
			local tUnits = SynthiqBotsUI.frames["MultiBar"].frames["Units"]
			for key, value in pairs(SynthiqBotsUI.index.actives) do
				if(tUnits.buttons[value].name ~= UnitName("player")) then
					tUnits.frames[value].getButton("Spellbook").setDisable()
				end
			end
			
			pButton.setEnable()
			SynthiqBotsUI.spellbook.name = pButton.getName()
			tUnits.buttons[SynthiqBotsUI.spellbook.name].waitFor = "SPELLBOOK"
			SendChatMessage("spells", "WHISPER", nil, pButton.getName())
		end
	end
	
	pFrame.addButton("Talent", 274, 0, "ability_marksmanship", SynthiqBotsUI.tips.every.talent).setDisable()
	.doLeft = function(pButton)
		if(pButton.state) then
			pButton.setDisable()
			SynthiqBotsUI.talent:Hide()
		elseif(UnitLevel(SynthiqBotsUI.toUnit(pButton.getName())) < 10) then
			SendChatMessage(SynthiqBotsUI.info.talent.Level, "SAY")
		elseif(CheckInteractDistance(SynthiqBotsUI.toUnit(pButton.getName()), 1) == nil) then
			SendChatMessage(SynthiqBotsUI.info.talent.OutOfRange, "SAY")
		else
			SynthiqBotsUI.talent:Hide()
			SynthiqBotsUI.talent.doClear()
			
			local tUnits = SynthiqBotsUI.frames["MultiBar"].frames["Units"]
			for key, value in pairs(SynthiqBotsUI.index.actives) do
				if(tUnits.buttons[value].name ~= UnitName("player")) then
					tUnits.frames[value].getButton("Talent").setDisable()
				end
			end
			
			InspectUnit(SynthiqBotsUI.toUnit(pButton.getName()))
			pButton.setEnable()
			
			SynthiqBotsUI.talent.name = pButton.getName()
			SynthiqBotsUI.talent.class = pButton.getClass()
			SynthiqBotsUI.auto.talent = true
		end
	end
	
	-- STRATEGIES --
	
	if(SynthiqBotsUI.isInside(pNormal, "food")) then pFrame.getButton("Food").setEnable() end
	if(SynthiqBotsUI.isInside(pNormal, "loot")) then pFrame.getButton("Loot").setEnable() end
	if(SynthiqBotsUI.isInside(pNormal, "gather")) then pFrame.getButton("Gather").setEnable() end
end