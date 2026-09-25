SynthiqBotsUI.addWarlock = function(pFrame, pCombat, pNormal)
	local tButton = pFrame.addButton("Buff", 0, 0, "spell_shadow_lifedrain02", SynthiqBotsUI.tips.warlock.buff.master)
	tButton.doLeft = function(pButton)
		SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Buff"])
	end
	
	local tFrame = pFrame.addFrame("Buff", -2, 30)
	tFrame:Hide()
	
	tFrame.addButton("BuffHealth", 0, 0, "spell_shadow_lifedrain02", SynthiqBotsUI.tips.warlock.buff.bhealth)
	.doLeft = function(pButton)
		SynthiqBotsUI.SelectToTarget(pButton.get(), "Buff", pButton.texture, "nc +bhealth,?", pButton.getName())
		pButton.getButton("Buff").doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +bhealth,?", "nc -bhealth,?", pButton.getName())
		end
	end
	
	tFrame.addButton("BuffMana", 0, 26, "spell_shadow_siphonmana", SynthiqBotsUI.tips.warlock.buff.bmana)
	.doLeft = function(pButton)
		SynthiqBotsUI.SelectToTarget(pButton.get(), "Buff", pButton.texture, "nc +bmana,?", pButton.getName())
		pButton.getButton("Buff").doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +bmana,?", "nc -bmana,?", pButton.getName())
		end
	end
	
	tFrame.addButton("BuffDps", 0, 52, "spell_shadow_haunting", SynthiqBotsUI.tips.warlock.buff.bdps)
	.doLeft = function(pButton)
		SynthiqBotsUI.SelectToTarget(pButton.get(), "Buff", pButton.texture, "nc +bdps,?", pButton.getName())
		pButton.getButton("Buff").doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +bdps,?", "nc -bdps,?", pButton.getName())
		end
	end
	
	-- STRATEGIES:BUFF --
	
	if(SynthiqBotsUI.isInside(pNormal, "bhealth")) then
		tButton.setTexture("spell_shadow_lifedrain02").setEnable().doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +bhealth,?", "nc -bhealth,?", pButton.getName())
		end
	elseif(SynthiqBotsUI.isInside(pNormal, "bmana")) then
		tButton.setTexture("spell_shadow_siphonmana").setEnable().doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +bmana,?", "nc -bmana,?", pButton.getName())
		end
	elseif(SynthiqBotsUI.isInside(pNormal, "bdps")) then
		tButton.setTexture("spell_shadow_haunting").setEnable().doRight = function(pButton)
			SynthiqBotsUI.OnOffActionToTarget(pButton, "nc +bdps,?", "nc -bdps,?", pButton.getName())
		end
	end
	
	-- DPS --
	
	pFrame.addButton("DpsControl", -30, 0, "ability_warrior_challange", SynthiqBotsUI.tips.warlock.dps.master)
	.doLeft = function(pButton)
		SynthiqBotsUI.ShowHideSwitch(pButton.getFrame("DpsControl"))
	end
	
	local tFrame = pFrame.addFrame("DpsControl", -32, 30)
	tFrame:Hide()
	
	tFrame.addButton("DpsAssist", 0, 0, "spell_holy_heroism", SynthiqBotsUI.tips.warlock.dps.dpsAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps assist,?", "co -dps assist,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	tFrame.addButton("DpsDebuff", 0, 26, "spell_holy_restoration", SynthiqBotsUI.tips.warlock.dps.dpsDebuff).setDisable()
	.doLeft = function(pButton)
		SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps debuff,?", "co -dps debuff,?", pButton.getName())
	end
	
	tFrame.addButton("DpsAoe", 0, 52, "spell_holy_surgeoflight", SynthiqBotsUI.tips.warlock.dps.dpsAoe).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps aoe,?", "co -dps aoe,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAssist").setDisable()
		end
	end
	
	tFrame.addButton("Dps", 0, 78, "spell_holy_divinepurpose", SynthiqBotsUI.tips.warlock.dps.dps).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps,?", "co -dps,?", pButton.getName())) then
			pButton.getButton("Tank").setDisable()
		end
	end
	
	-- ASSIST --
	
	pFrame.addButton("TankAssist", -60, 0, "ability_warrior_innerrage", SynthiqBotsUI.tips.warlock.tankAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +tank assist,?", "co -tank assist,?", pButton.getName())) then
			pButton.getButton("DpsAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	-- TANK --
	
	pFrame.addButton("Tank", -90, 0, "ability_warrior_shieldmastery", SynthiqBotsUI.tips.warlock.tank).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +tank,?", "co -tank,?", pButton.getName())) then
			pButton.getButton("Dps").setDisable()
		end
	end
	
	-- STRATEGIES --
	
	if(SynthiqBotsUI.isInside(pCombat, "dps")) then pFrame.getButton("Dps").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "dps aoe")) then pFrame.getButton("DpsAoe").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "dps debuff")) then pFrame.getButton("DpsDebuff").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "dps assist")) then pFrame.getButton("DpsAssist").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "tank assist")) then pFrame.getButton("TankAssist").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "tank")) then pFrame.getButton("Tank").setEnable() end
end