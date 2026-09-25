SynthiqBotsUI.addDeathKnight = function(pFrame, pCombat, pNormal)
	local tButton = pFrame.addButton("Presence", 0, 0, "spell_deathknight_bloodpresence", SynthiqBotsUI.tips.deathknight.presence.master).setDisable()
	tButton.doLeft = function(pButton)
		SynthiqBotsUI.ShowHideSwitch(pButton.getFrame("Presence"))
	end
	
	local tFrame = pFrame.addFrame("Presence", -2, 30)
	tFrame:Hide()
	
	tFrame.addButton("Unholy", 0, 0, "spell_deathknight_unholypresence", SynthiqBotsUI.tips.deathknight.presence.unholy)
	.doLeft = function(pButton)
		SynthiqBotsUI.SelectToTarget(pButton.get(), "Presence", pButton.texture, "co +unholy,?", pButton.getName())
		pButton.getButton("Presence").doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "co +unholy,?", "co -unholy,?", pButton.getName())
		end
	end
	
	tFrame.addButton("Frost", 0, 26, "spell_deathknight_frostpresence", SynthiqBotsUI.tips.deathknight.presence.frost)
	.doLeft = function(pButton)
		SynthiqBotsUI.SelectToTarget(pButton.get(), "Presence", pButton.texture, "co +frost,?", pButton.getName())
		pButton.getButton("Presence").doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "co +frost,?", "co -frost,?", pButton.getName())
		end
	end
	
	tFrame.addButton("Blood", 0, 52, "spell_deathknight_bloodpresence", SynthiqBotsUI.tips.deathknight.presence.blood)
	.doLeft = function(pButton)
		SynthiqBotsUI.SelectToTarget(pButton.get(), "Presence", pButton.texture, "co +blood,?", pButton.getName())
		pButton.getButton("Presence").doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "co +blood,?", "co -blood,?", pButton.getName())
		end
	end
	
	-- SRATEGIES:PRESENCE ---
	
	if(SynthiqBotsUI.isInside(pCombat, "unholy")) then
		tButton.setTexture("spell_deathknight_unholypresence").setEnable().doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "co +unholy,?", "co -unholy,?", pButton.getName())
		end
	elseif(SynthiqBotsUI.isInside(pCombat, "frost")) then
		tButton.setTexture("spell_deathknight_frostpresence").setEnable().doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "co +frost,?", "co -frost,?", pButton.getName())
		end
	elseif(SynthiqBotsUI.isInside(pCombat, "blood")) then
		tButton.setTexture("spell_deathknight_bloodpresence").setEnable().doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "co +blood,?", "co -blood,?", pButton.getName())
		end
	end
	
	-- DPS --
	
	pFrame.addButton("DpsControl", -30, 0, "ability_warrior_challange", SynthiqBotsUI.tips.deathknight.dps.master)
	.doLeft = function(pButton)
		SynthiqBotsUI.ShowHideSwitch(pButton.getFrame("DpsControl"))
	end
	
	local tFrame = pFrame.addFrame("DpsControl", -32, 30)
	tFrame:Hide()
	
	tFrame.addButton("DpsAssist", 0, 0, "spell_holy_heroism", SynthiqBotsUI.tips.deathknight.dps.dpsAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps assist,?", "co -dps assist,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	tFrame.addButton("DpsAoe", 0, 26, "spell_holy_surgeoflight", SynthiqBotsUI.tips.deathknight.dps.dpsAoe).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps aoe,?", "co -dps aoe,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAssist").setDisable()
		end
	end
	
	-- ASSIST --
	
	pFrame.addButton("TankAssist", -60, 0, "ability_warrior_innerrage", SynthiqBotsUI.tips.deathknight.tankAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +tank assist,?", "co -tank assist,?", pButton.getName())) then
			pButton.getButton("DpsAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	-- STRATEGIES --
	
	if(SynthiqBotsUI.isInside(pCombat, "dps aoe")) then pFrame.getButton("DpsAoe").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "dps assist")) then pFrame.getButton("DpsAssist").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "tank assist")) then pFrame.getButton("TankAssist").setEnable() end
end