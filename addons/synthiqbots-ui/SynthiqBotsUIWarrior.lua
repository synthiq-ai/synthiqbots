SynthiqBotsUI.addWarrior = function(pFrame, pCombat, pNormal)
	pFrame.addButton("DpsControl", 0, 0, "ability_warrior_challange", SynthiqBotsUI.tips.warrior.dps.master)
	.doLeft = function(pButton)
		SynthiqBotsUI.ShowHideSwitch(pButton.getFrame("DpsControl"))
	end
	
	local tFrame = pFrame.addFrame("DpsControl", -2, 30)
	tFrame:Hide()
	
	tFrame.addButton("DpsAssist", 0, 0, "spell_holy_heroism", SynthiqBotsUI.tips.warrior.dps.dpsAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps assist,?", "co -dps assist,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	tFrame.addButton("DpsAoe", 0, 26, "spell_holy_surgeoflight", SynthiqBotsUI.tips.warrior.dps.dpsAoe).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps aoe,?", "co -dps aoe,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAssist").setDisable()
		end
	end
	
	-- ASSIST --
	
	pFrame.addButton("TankAssist", -30, 0, "ability_warrior_innerrage", SynthiqBotsUI.tips.warrior.tankAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +tank assist,?", "co -tank assist,?", pButton.getName())) then
			pButton.getButton("DpsAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	-- TANK --
	
	pFrame.addButton("Tank", -60, 0, "ability_warrior_shieldmastery", SynthiqBotsUI.tips.warrior.tank).setDisable()
	.doLeft = function(pButton)
		SynthiqBotsUI.OnOffActionToTarget(pButton, "co +tank,?", "co -tank,?", pButton.getName())
	end
	
	-- STRATEGIES --
	
	if(SynthiqBotsUI.isInside(pCombat, "dps aoe")) then pFrame.getButton("DpsAoe").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "dps assist")) then pFrame.getButton("DpsAssist").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "tank assist")) then pFrame.getButton("TankAssist").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "tank")) then pFrame.getButton("Tank").setEnable() end
end